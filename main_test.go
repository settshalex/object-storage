package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/gorilla/mux"
	"github.com/minio/minio-go/v7"
	"github.com/rs/zerolog"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ========================= START MOCK IMPLEMENTATION ================================
// Mock Minio Client for testing
type mockMinioClient struct{}

func (m *mockMinioClient) PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	return minio.UploadInfo{
		Bucket: bucketName,
		Size:   objectSize,
	}, nil
}

func (m *mockMinioClient) GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (*minio.Object, error) {
	return &minio.Object{}, nil
}

func (m *mockMinioClient) MakeBucket(ctx context.Context, bucketName string, opts minio.MakeBucketOptions) error {
	return nil
}

func (m *mockMinioClient) BucketExists(ctx context.Context, bucketName string) (bool, error) {
	return true, nil
}

func newTestGateway() *MinioClients {
	minioClients := []MinioClientInterface{
		&mockMinioClient{},
	}

	return &MinioClients{minioClients: minioClients}
}

// ========================= FINISH MOCK IMPLEMENTATION ================================

// Disable log
func init() {
	zerolog.SetGlobalLevel(zerolog.Disabled)
}

// Test PUT /object/{id:[a-zA-Z0-9-_]{1,32}}
func TestPutObjectHandler(t *testing.T) {
	gateway := newTestGateway()

	r := mux.NewRouter()
	r.HandleFunc("/object/{id:[a-zA-Z0-9-_]{1,32}}", gateway.putObjectHandler).Methods("PUT")
	r.Use(loggingMiddleware)
	r.Use(RecoveryMiddleware)

	server := httptest.NewServer(r)
	defer server.Close()

	jsonData := []byte(`{"data":"testdata"}`)
	req, err := http.NewRequest("PUT", server.URL+"/object/testid", bytes.NewBuffer(jsonData))
	if err != nil {
		t.Fatalf("could not create request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("could not send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status OK; got %v", resp.Status)
	}

	var response responsePutObject
	err = json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		t.Fatalf("could not decode response: %v", err)
	}

	if response.Bucket != "bucket" {
		t.Errorf("expected bucket to be 'bucket'; got %v", response.Bucket)
	}
}

// Test GET /object/{id:[a-zA-Z0-9-_]{1,32}}
func TestGetObjectHandler(t *testing.T) {
	gateway := newTestGateway()

	r := mux.NewRouter()
	// TODO at the moment GetObject mock return an empty Object that raise an error
	r.HandleFunc("/object/{id:[a-zA-Z0-9-_]{1,32}}", gateway.getObjectHandler).Methods("GET")
	r.Use(loggingMiddleware)
	r.Use(RecoveryMiddleware)

	server := httptest.NewServer(r)
	defer server.Close()

	req, err := http.NewRequest("GET", server.URL+"/object/testid", nil)
	if err != nil {
		t.Fatalf("could not create request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("could not send request: %v", err)
	}
	defer resp.Body.Close()
	// TODO when it's fixed GetObject mock change status to check
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status OK; got %v", resp.Status)
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	recoveryHandler := RecoveryMiddleware(handler)

	req, err := http.NewRequest("GET", "/", nil)
	if err != nil {
		t.Fatalf("could not create request: %v", err)
	}

	rr := httptest.NewRecorder()
	recoveryHandler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusInternalServerError {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusInternalServerError)
	}

	expected := http.StatusText(http.StatusInternalServerError)
	readBody := strings.TrimSuffix(rr.Body.String(), "\n")
	if readBody != expected {
		t.Errorf("handler returned unexpected body: got '%v' want '%v'", rr.Body.String(), expected)
	}
}
