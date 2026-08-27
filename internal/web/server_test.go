package web

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"pdreview/internal/application"
	"pdreview/internal/assessment"
	"pdreview/internal/store"
)

func testHTTPServer(t *testing.T) (*httptest.Server, *store.Ledger) {
	t.Helper()
	ledger, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewService(ledger, assessment.MustDefault())
	if err != nil {
		t.Fatal(err)
	}
	webServer, err := NewServer(service, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(webServer.Handler()), ledger
}

func TestWorkbenchAndStructuredVersionConflict(t *testing.T) {
	server, ledger := testHTTPServer(t)
	defer server.Close()
	defer ledger.Close()
	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(b, []byte("<body>")) || !bytes.Contains(b, []byte("局部放电放行工作台")) {
		t.Fatalf("工作台 HTML 不完整: %d %s", response.StatusCode, b)
	}
	draft := map[string]any{"cableSegment": "测试段", "insulationStructure": "XLPE", "ratedVoltageKv": 220, "testPlan": "测试方案", "operator": "试验员"}
	created := apiCall(t, server.URL+"/api/campaigns", "create", draft)
	var aggregate struct {
		Campaign struct {
			ID string `json:"id"`
		} `json:"campaign"`
	}
	if err := json.Unmarshal(created.Data, &aggregate); err != nil {
		t.Fatal(err)
	}
	command := map[string]any{"expectedVersion": 99, "actor": "试验员", "measurements": []any{}}
	payload, _ := json.Marshal(command)
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/campaigns/"+aggregate.Campaign.ID+"/measurements", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "conflict")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Error struct {
			Code           string `json:"code"`
			CurrentVersion int64  `json:"currentVersion"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusConflict || envelope.Error.Code != "version_conflict" || envelope.Error.CurrentVersion != 1 {
		t.Fatalf("版本冲突响应不完整: %d %#v", response.StatusCode, envelope)
	}
}

type testEnvelope struct {
	Data json.RawMessage `json:"data"`
}

func apiCall(t *testing.T, url, key string, input any) testEnvelope {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("API 返回 %d: %s", response.StatusCode, body)
	}
	var envelope testEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}
