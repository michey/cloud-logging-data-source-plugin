// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/GoogleCloudPlatform/cloud-logging-grafana-data-source-plugin/pkg/plugin/cloudlogging"
)

// Make sure CloudLoggingDatasource implements required interfaces
var (
	_                     backend.QueryDataHandler      = (*CloudLoggingDatasource)(nil)
	_                     backend.CheckHealthHandler    = (*CloudLoggingDatasource)(nil)
	_                     instancemgmt.InstanceDisposer = (*CloudLoggingDatasource)(nil)
	errMissingCredentials                               = errors.New("missing credentials")
)

const (
	privateKeyKey = "privateKey"
)

// jwtToken is the fields parsed from a JWT token and sent from the front end
type jwtToken struct {
	AuthType       string `json:"authenticationType"`
	ClientEmail    string `json:"clientEmail"`
	DefaultProject string `json:"defaultProject"`
	TokenURI       string `json:"tokenUri"`
}

// toServiceAccountJSON creates the serviceAccountJSON bytes from the JWT token fields
// in gcpCreds
func (c jwtToken) toServiceAccountJSON(privateKey string) ([]byte, error) {
	return json.Marshal(serviceAccountJSON{
		Type:        "service_account",
		ProjectID:   c.DefaultProject,
		PrivateKey:  privateKey,
		ClientEmail: c.ClientEmail,
		TokenURI:    c.TokenURI,
	})
}

// serviceAccountJSON is the expected structure of a GCP Service Account credentials file
// We mainly want to be able to pull out ProjectID to use as a default
type serviceAccountJSON struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

// NewCloudLoggingDatasource creates a new datasource instance.
func NewCloudLoggingDatasource(settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	var creds jwtToken
	if err := json.Unmarshal(settings.JSONData, &creds); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	privateKey, ok := settings.DecryptedSecureJSONData[privateKeyKey]
	if !ok || privateKey == "" {
		return nil, errMissingCredentials
	}

	serviceAccount, err := creds.toServiceAccountJSON(privateKey)
	if err != nil {
		return nil, fmt.Errorf("create credentials: %w", err)
	}

	client, err := cloudlogging.NewClient(context.TODO(), serviceAccount)
	if err != nil {
		return nil, err
	}

	return &CloudLoggingDatasource{
		client: client,
	}, nil
}

// CloudLoggingDatasource is an example datasource which can respond to data queries, reports
// its health and has streaming skills.
type CloudLoggingDatasource struct {
	client cloudlogging.API
}

// Dispose here tells plugin SDK that plugin wants to clean up resources when a new instance
// created. As soon as datasource settings change detected by SDK old datasource instance will
// be disposed and a new one will be created using NewSampleDatasource factory function.
func (d *CloudLoggingDatasource) Dispose() {
	if err := d.client.Close(); err != nil {
		log.DefaultLogger.Error("failed closing client", "error", err)
	}
}

// ListProjectsResponse is our response to a call to `/resources/projects`
type ListProjectsResponse struct {
	Projects []string `json:"projects"`
}

// CallResource fetches some resource from GCP using the data source's credentials
//
// Currently only projects are fetched, other requests receive a 404
func (d *CloudLoggingDatasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	log.DefaultLogger.Info("CallResource called")

	// Right now we only support calls to `/projects`
	resource := req.Path
	if strings.ToLower(resource) != "projects" {
		return sender.Send(&backend.CallResourceResponse{
			Status: http.StatusNotFound,
			Body:   []byte(`No such path`),
		})
	}

	projects, err := d.client.ListProjects(ctx)
	if err != nil {
		log.DefaultLogger.Error("error listing", "error", err)
		return sender.Send(&backend.CallResourceResponse{
			Status: http.StatusInternalServerError,
			Body:   []byte(`Unable to list projects`),
		})
	}

	body, err := json.Marshal(&ListProjectsResponse{Projects: projects})
	if err != nil {
		return sender.Send(&backend.CallResourceResponse{
			Status: http.StatusInternalServerError,
			Body:   []byte(`Unable to create response`),
		})
	}

	return sender.Send(&backend.CallResourceResponse{
		Status: http.StatusOK,
		Body:   body,
	})

}

// QueryData handles multiple queries and returns multiple responses.
// req contains the queries []DataQuery (where each query contains RefID as a unique identifier).
// The QueryDataResponse contains a map of RefID to the response for each query, and each response
// contains Frames ([]*Frame).
func (d *CloudLoggingDatasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	log.DefaultLogger.Info("QueryData called")

	// create response struct
	response := backend.NewQueryDataResponse()

	// loop over queries and execute them individually.
	for _, q := range req.Queries {
		res := d.query(ctx, req.PluginContext, q)

		// save the response in a hashmap
		// based on with RefID as identifier
		response.Responses[q.RefID] = res
	}

	return response, nil
}

// queryModel is the fields needed to query from Grafana
type queryModel struct {
	QueryText     string `json:"queryText"`
	ProjectID     string `json:"projectId"`
	MaxDataPoints int    `json:"MaxDataPoints"`
}

func (d *CloudLoggingDatasource) query(ctx context.Context, pCtx backend.PluginContext, query backend.DataQuery) backend.DataResponse {
	response := backend.DataResponse{}

	var q queryModel
	response.Error = json.Unmarshal(query.JSON, &q)
	if response.Error != nil {
		return response
	}

	clientRequest := cloudlogging.Query{
		ProjectID: q.ProjectID,
		Filter:    q.QueryText,
		Limit:     query.MaxDataPoints,
		TimeRange: struct {
			From string
			To   string
		}{
			From: query.TimeRange.From.Format(time.RFC3339),
			To:   query.TimeRange.To.Format(time.RFC3339),
		},
	}

	logs, err := d.client.ListLogs(ctx, &clientRequest)
	if err != nil {
		response.Error = fmt.Errorf("query: %w", err)
		return response
	}

	// create data frame response.
	frames := []*data.Frame{}

	for i := 0; i < len(logs); i++ {
		body, err := cloudlogging.GetLogEntryMessage(logs[i])
		if err != nil {
			log.DefaultLogger.Warn("failed getting log message", "error", err)
			continue
		}

		labels := cloudlogging.GetLogLabels(logs[i])
		f := data.NewFrame(logs[i].GetInsertId())
		timestamp := data.NewField("time", nil, []time.Time{logs[i].GetTimestamp().AsTime()})
		content := data.NewField("content", labels, []string{body})

		f.Fields = append(f.Fields, timestamp, content)
		f.Meta = &data.FrameMeta{}
		f.Meta.PreferredVisualization = data.VisTypeLogs
		frames = append(frames, f)
	}

	// add the frames to the response.
	for _, f := range frames {
		response.Frames = append(response.Frames, f)
	}

	return response
}

// CheckHealth handles health checks sent from Grafana to the plugin.
// The main use case for these health checks is the test button on the
// datasource configuration page which allows users to verify that
// a datasource is working as expected.
func (d *CloudLoggingDatasource) CheckHealth(ctx context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	log.DefaultLogger.Info("CheckHealth called")

	var status = backend.HealthStatusOk
	settings := req.PluginContext.DataSourceInstanceSettings

	var creds jwtToken
	if err := json.Unmarshal(settings.JSONData, &creds); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	privateKey, ok := settings.DecryptedSecureJSONData[privateKeyKey]
	if !ok || privateKey == "" {
		return nil, errMissingCredentials
	}

	serviceAccount, err := creds.toServiceAccountJSON(privateKey)
	if err != nil {
		return nil, fmt.Errorf("create credentials: %w", err)
	}

	client, err := cloudlogging.NewClient(ctx, serviceAccount)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err := client.Close(); err != nil {
			log.DefaultLogger.Warn("failed closing client", "error", err)
		}
	}()

	if err := client.TestConnection(ctx, creds.DefaultProject); err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("failed to run test query: %s", err),
		}, nil
	}

	return &backend.CheckHealthResult{
		Status:  status,
		Message: fmt.Sprintf("Successfully queried logs from GCP project %s", creds.DefaultProject),
	}, nil
}