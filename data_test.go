package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func dataTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := authTestServer(t)
	u, err := s.auth.createUser("data@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.auth.createSession(u.ID, "data-test")
	if err != nil {
		t.Fatal(err)
	}
	return s, token
}

func createDataNamespace(t *testing.T, s *Server, token, body string) string {
	t.Helper()
	created := vectorRequest(s, token, "POST", "/v1/data/namespaces", body)
	if created.Response.StatusCode() != 201 {
		t.Fatalf("create database: %d %s", created.Response.StatusCode(), created.Response.Body())
	}
	var response struct {
		Database struct {
			ID string `json:"id"`
		} `json:"database"`
	}
	if err := json.Unmarshal(created.Response.Body(), &response); err != nil || response.Database.ID == "" {
		t.Fatalf("decode database: %v %s", err, created.Response.Body())
	}
	return response.Database.ID
}

func TestManagedDataCRUDAndTenantIsolation(t *testing.T) {
	s, token := dataTestServer(t)
	databaseID := createDataNamespace(t, s, token, `{"name":"Customers"}`)

	saved := vectorRequest(s, token, "PUT", "/v1/data/namespaces/"+databaseID+"/records",
		`{"key":"customer:123","value":{"name":"Ada","plan":"starter"}}`)
	if saved.Response.StatusCode() != 200 || !bytes.Contains(saved.Response.Body(), []byte(`"status":"not_linked"`)) {
		t.Fatalf("save record: %d %s", saved.Response.StatusCode(), saved.Response.Body())
	}
	listed := vectorRequest(s, token, "GET", "/v1/data/namespaces/"+databaseID+"/records", "")
	if listed.Response.StatusCode() != 200 || !bytes.Contains(listed.Response.Body(), []byte(`"customer:123"`)) ||
		!bytes.Contains(listed.Response.Body(), []byte(`"plan":"starter"`)) {
		t.Fatalf("list records: %d %s", listed.Response.StatusCode(), listed.Response.Body())
	}

	other, err := s.auth.createUser("other-data@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	otherToken, _ := s.auth.createSession(other.ID, "other-data-test")
	foreign := vectorRequest(s, otherToken, "GET", "/v1/data/namespaces/"+databaseID+"/records", "")
	if foreign.Response.StatusCode() != 404 {
		t.Fatalf("cross-tenant read status=%d body=%s", foreign.Response.StatusCode(), foreign.Response.Body())
	}

	deleted := vectorRequest(s, token, "DELETE", "/v1/data/namespaces/"+databaseID+"/records",
		`{"key":"customer:123"}`)
	if deleted.Response.StatusCode() != 200 {
		t.Fatalf("delete record: %d %s", deleted.Response.StatusCode(), deleted.Response.Body())
	}
	listed = vectorRequest(s, token, "GET", "/v1/data/namespaces/"+databaseID+"/records", "")
	if !bytes.Contains(listed.Response.Body(), []byte(`"records":[]`)) {
		t.Fatalf("deleted record remains: %s", listed.Response.Body())
	}
}

func TestManagedDataCanIndexIntoOwnedVectorProject(t *testing.T) {
	s, token, upserts := vectorTestServer(t)
	createdVector := vectorRequest(s, token, "POST", "/v1/vector/projects", `{"name":"Customer search"}`)
	var vectorResponse struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := json.Unmarshal(createdVector.Response.Body(), &vectorResponse); err != nil {
		t.Fatal(err)
	}
	databaseID := createDataNamespace(t, s, token,
		`{"name":"Customers","vector_project_id":"`+vectorResponse.Project.ID+`"}`)
	saved := vectorRequest(s, token, "PUT", "/v1/data/namespaces/"+databaseID+"/records",
		`{"key":"customer:123","value":{"name":"Ada"},"search_text":"Ada builds services with Mojo"}`)
	if saved.Response.StatusCode() != 200 || !bytes.Contains(saved.Response.Body(), []byte(`"status":"ready"`)) {
		t.Fatalf("vector-linked save: %d %s", saved.Response.StatusCode(), saved.Response.Body())
	}
	if upserts.Load() != 1 {
		t.Fatalf("vector worker got %d upserts, want one", upserts.Load())
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM vector_documents WHERE project_id=$1 AND external_id LIKE 'data:%'`,
		vectorResponse.Project.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("linked vector documents=%d err=%v", count, err)
	}
	deleted := vectorRequest(s, token, "DELETE", "/v1/data/namespaces/"+databaseID+"/records",
		`{"key":"customer:123"}`)
	if deleted.Response.StatusCode() != 200 || !bytes.Contains(deleted.Response.Body(), []byte(`"status":"ready"`)) {
		t.Fatalf("vector-linked delete: %d %s", deleted.Response.StatusCode(), deleted.Response.Body())
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM vector_documents WHERE project_id=$1 AND external_id LIKE 'data:%'`,
		vectorResponse.Project.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted linked vector documents=%d err=%v", count, err)
	}
}
