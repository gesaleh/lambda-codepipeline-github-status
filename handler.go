package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/codepipeline"
)

type event struct {
	ExecutionID string `json:"execution-id"`
	GithubToken string `json:"github-token"`
	Pipeline    string `json:"pipeline"`
}

type ghReqPayload struct {
	State       string `json:"state"`
	TargetURL   string `json:"target_url"`
	Description string `json:"description"`
	Context     string `json:"context"`
}

// HandleLambdaEvent is triggered by a CloudWatch event rule.
func HandleLambdaEvent(ev event) error {
	if ev.ExecutionID == "" {
		return errors.New("missing event param execution-id")
	}
	if ev.GithubToken == "" {
		return errors.New("missing event param github-token")
	}
	if ev.Pipeline == "" {
		return errors.New("missing event param pipeline")
	}

	sess := session.Must(session.NewSession())
	cpSvc := codepipeline.New(sess)
	res, err := cpSvc.GetPipelineExecution(&codepipeline.GetPipelineExecutionInput{
		PipelineExecutionId: aws.String(ev.ExecutionID),
		PipelineName:        aws.String(ev.Pipeline),
	})
	if err != nil {
		return err
	}

	var sourceArti *codepipeline.ArtifactRevision
	for _, a := range res.PipelineExecution.ArtifactRevisions {
		if aws.StringValue(a.Name) == "SourceArtifact" {
			sourceArti = a
			break
		}
	}
	if sourceArti == nil {
		return errors.New("missing SourceArtifact")
	}

	rev := aws.StringValue(sourceArti.RevisionId)
	url, err := url.Parse(aws.StringValue(sourceArti.RevisionUrl))
	if err != nil {
		return err
	}
	log.Printf("revision ID: %v URL: %v\n", rev, url)

	status := aws.StringValue(res.PipelineExecution.Status)
	var ghStatus string
	switch status {
	case "InProgress":
		ghStatus = "pending"
	case "Succeeded":
		ghStatus = "success"
	default:
		ghStatus = "failure"
	}

	repo, err := extractRepoName(url)
	if err != nil {
		return fmt.Errorf("failed to extract repo name from artifact url %v: %w", url, err)
	}

	deepLink := fmt.Sprintf(
		"https://%s.console.aws.amazon.com/codesuite/codepipeline/pipelines/%s/executions/%s",
		"eu-west-1", ev.Pipeline, ev.ExecutionID)
	ghURL := fmt.Sprintf("https://api.github.com/repos/%s/statuses/%s", repo, rev)

	log.Printf("Setting status for repo=%s, commit=%s to %s\n", repo, rev, ghStatus)

	var b bytes.Buffer
	err = json.NewEncoder(&b).Encode(ghReqPayload{
		State:     ghStatus,
		TargetURL: deepLink,
		Context:   "continuous-integration/codepipeline",
	})
	if err != nil {
		return err
	}

	ghReq, err := http.NewRequest("POST", ghURL, &b)
	if err != nil {
		return err
	}
	ghReq.Header.Set("Accept", "application/json")
	ghReq.Header.Set("Authorization", "token "+ev.GithubToken)
	ghReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	client := &http.Client{}
	ghRes, err := client.Do(ghReq)
	if err != nil {
		return err
	}
	defer ghRes.Body.Close()
	if ghRes.StatusCode != 201 {
		resBody, _ := ioutil.ReadAll(ghRes.Body)
		return fmt.Errorf("unexpected response from GitHub: %d body: %s",
			ghRes.StatusCode, string(resBody))
	}

	return nil
}

func extractRepoName(url *url.URL) (string, error) {
	switch url.Hostname() {
	case "github.com":
		p := strings.Split(url.Path, "/")
		if len(p) < 3 {
			return "", fmt.Errorf("too few path components")
		}
		return fmt.Sprintf("%s/%s", p[1], p[2]), nil
	case "eu-west-1.console.aws.amazon.com":
		if url.Path != "/codesuite/settings/connections/redirect" {
			return "", fmt.Errorf("unexpected URL path: %v", url.Path)
		}
		repo := url.Query().Get("FullRepositoryId")
		if repo == "" {
			return "", fmt.Errorf("missing FullRepositoryId URL param")
		}
		return repo, nil
	default:
		return "", fmt.Errorf("unknown hostname %v", url.Hostname())
	}
}
