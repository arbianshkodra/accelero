package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWebhook(t *testing.T) {
	taskQueue := make(chan WebhookTask, 1)

	req, err := http.NewRequest("POST", "/webhook", bytes.NewBuffer([]byte("{}")))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handlerFunc := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Webhook(w, r, taskQueue)
	})

	handlerFunc.ServeHTTP(rr, req)

	// Log the status code and response body
	t.Logf("Status Code: %d", rr.Code)
	t.Logf("Response Body: %s", rr.Body.String())

	// Check the status code
	assert.Equal(t, http.StatusAccepted, rr.Code)

	// Parse the JSON response
	var responseMap map[string]string
	err = json.Unmarshal(rr.Body.Bytes(), &responseMap)
	assert.NoError(t, err)

	// Check that the response contains the expected keys and values
	assert.Equal(t, "accepted", responseMap["status"])
	assert.NotEmpty(t, responseMap["request_id"])

	// Check that a task was enqueued
	select {
	case task := <-taskQueue:
		t.Logf("Enqueued Task: %+v", task)
		assert.NotEmpty(t, task.RequestID)
	default:
		t.Error("Expected a task to be enqueued")
	}
}
