package gateway

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestContextSurvivesGinReuse(t *testing.T) {
	type key struct{}
	request, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "request-scope"))
	defer cancel()
	pooled := &gin.Context{Request: httptest.NewRequest("GET", "/", nil).WithContext(request)}
	service := apiRequestContext(pooled)
	pooled.Request = httptest.NewRequest("GET", "/next", nil)
	cancel()
	if service.Err() != context.Canceled || service.Value(key{}) != "request-scope" {
		t.Fatal("reusing Gin lost the original request cancellation or scope")
	}
}
