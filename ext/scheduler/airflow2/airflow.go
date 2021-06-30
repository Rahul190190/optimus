package airflow2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/odpf/optimus/models"
	"github.com/odpf/optimus/store"
	"github.com/pkg/errors"

	_ "embed"
)

//go:embed resources/__lib.py
var resSharedLib []byte

//go:embed resources/base_dag.py
var resBaseDAG []byte

const (
	baseLibFileName = "__lib.py"
	dagStatusUrl    = "api/v1/dags/%s/dagRuns"
	dagRunClearURL  = "api/v1/dags/%s/clearTaskInstances"
)

type HttpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type ObjectWriterFactory interface {
	New(ctx context.Context, writerPath, writerSecret string) (store.ObjectWriter, error)
}

type scheduler struct {
	objWriterFac ObjectWriterFactory
	httpClient   HttpClient
}

func NewScheduler(ow ObjectWriterFactory, httpClient HttpClient) *scheduler {
	return &scheduler{
		objWriterFac: ow,
		httpClient:   httpClient,
	}
}

func (a *scheduler) GetName() string {
	return "airflow2"
}

func (a *scheduler) GetJobsDir() string {
	return "dags"
}

func (a *scheduler) GetJobsExtension() string {
	return ".py"
}

func (a *scheduler) GetTemplate() []byte {
	return resBaseDAG
}

func (a *scheduler) Bootstrap(ctx context.Context, proj models.ProjectSpec) error {
	storagePath, ok := proj.Config[models.ProjectStoragePathKey]
	if !ok {
		return errors.Errorf("%s config not configured for project %s", models.ProjectStoragePathKey, proj.Name)
	}
	storageSecret, ok := proj.Secret.GetByName(models.ProjectSecretStorageKey)
	if !ok {
		return errors.Errorf("%s secret not configured for project %s", models.ProjectSecretStorageKey, proj.Name)
	}

	p, err := url.Parse(storagePath)
	if err != nil {
		return err
	}
	objectWriter, err := a.objWriterFac.New(ctx, storagePath, storageSecret)
	if err != nil {
		return errors.Errorf("object writer failed for %s", proj.Name)
	}
	return a.migrateLibFileToWriter(ctx, objectWriter, p.Hostname(), filepath.Join(strings.Trim(p.Path, "/"), a.GetJobsDir(), baseLibFileName))
}

func (a *scheduler) migrateLibFileToWriter(ctx context.Context, objWriter store.ObjectWriter, bucket, objPath string) (err error) {
	// copy to fs
	dst, err := objWriter.NewWriter(ctx, bucket, objPath)
	if err != nil {
		return err
	}
	defer func() {
		if derr := dst.Close(); derr != nil {
			if err == nil {
				err = derr
			} else {
				err = errors.Wrap(err, derr.Error())
			}
		}
	}()

	_, err = io.Copy(dst, bytes.NewBuffer(resSharedLib))
	return
}

func (a *scheduler) GetJobStatus(ctx context.Context, projSpec models.ProjectSpec, jobName string) ([]models.JobStatus,
	error) {
	schdHost, ok := projSpec.Config[models.ProjectSchedulerHost]
	if !ok {
		return nil, errors.Errorf("scheduler host not set for %s", projSpec.Name)
	}
	schdHost = strings.Trim(schdHost, "/")

	fetchUrl := fmt.Sprintf(fmt.Sprintf("%s/%s", schdHost, dagStatusUrl), jobName)
	request, err := http.NewRequest(http.MethodGet, fetchUrl, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to build http request for %s", fetchUrl)
	}

	resp, err := a.httpClient.Do(request)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to fetch airflow dag runs from %s", fetchUrl)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Errorf("failed to fetch airflow dag runs from %s", fetchUrl)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read airflow response")
	}

	//{
	//	"dag_runs": [
	//		{
	//			"dag_id": "",
	//			"dag_run_url": "",
	//			"execution_date": "2020-03-25T02:00:00+00:00",
	//			"id": 1997,
	//			"run_id": "scheduled__2020-03-25T02:00:00+00:00",
	//			"start_date": "2020-06-01T16:32:58.489042+00:00",
	//			"state": "success"
	//	   },
	//	],
	//	"total_entries": 0
	//}
	var responseJson struct {
		DagRuns []map[string]interface{} `json:"dag_runs"`
	}
	err = json.Unmarshal(body, &responseJson)
	if err != nil {
		return nil, errors.Wrapf(err, "json error: %s", string(body))
	}

	jobStatus := []models.JobStatus{}
	for _, status := range responseJson.DagRuns {
		_, ok1 := status["execution_date"]
		_, ok2 := status["state"]
		if !ok1 || !ok2 {
			return nil, errors.Errorf("failed to find required response fields %s in %s", jobName, status)
		}
		schdAt, err := time.Parse(models.InstanceScheduledAtTimeLayout, status["execution_date"].(string))
		if err != nil {
			return nil, errors.Errorf("error parsing date for %s, %s", jobName, status["execution_date"].(string))
		}
		jobStatus = append(jobStatus, models.JobStatus{
			ScheduledAt: schdAt,
			State:       models.JobStatusState(status["state"].(string)),
		})
	}

	return jobStatus, nil
}

func (a *scheduler) Clear(ctx context.Context, projSpec models.ProjectSpec, jobName string, startDate, endDate time.Time) error {
	schdHost, ok := projSpec.Config[models.ProjectSchedulerHost]
	if !ok {
		return errors.Errorf("scheduler host not set for %s", projSpec.Name)
	}

	schdHost = strings.Trim(schdHost, "/")
	airflowDateFormat := "2006-01-02T15:04:05+00:00"
	var jsonStr = []byte(fmt.Sprintf(`{"start_date":"%s", "end_date": "%s", "dry_run": false}`,
		startDate.UTC().Format(airflowDateFormat),
		endDate.UTC().Format(airflowDateFormat)))
	postURL := fmt.Sprintf(
		fmt.Sprintf("%s/%s", schdHost, dagRunClearURL),
		jobName)

	request, err := http.NewRequest(http.MethodPost, postURL, bytes.NewBuffer(jsonStr))
	if err != nil {
		return errors.Wrapf(err, "failed to build http request for %s", postURL)
	}
	request.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(request)
	if err != nil {
		return errors.Wrapf(err, "failed to clear airflow dag runs from %s", postURL)
	}
	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("failed to clear airflow dag runs from %s", postURL)
	}
	defer resp.Body.Close()

	return nil
}