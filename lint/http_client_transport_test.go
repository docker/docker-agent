package main

import (
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
)

func TestHTTPClientTransportFlagsBareClient(t *testing.T) {
	t.Parallel()
	src := `package mypackage
import "net/http"
func doSomething() *http.Client {
	return &http.Client{}
}
`
	offenses := coptest.RunNamed(t, HTTPClientTransport, "pkg/foo/foo.go", src)
	assert.Len(t, offenses, 1)
	assert.Equal(t, "Lint/HTTPClientTransport", offenses[0].CopName)
}

func TestHTTPClientTransportFlagsClientWithTimeoutOnly(t *testing.T) {
	t.Parallel()
	src := `package mypackage
import (
	"net/http"
	"time"
)
var c = &http.Client{Timeout: 30 * time.Second}
`
	assert.Len(t, coptest.RunNamed(t, HTTPClientTransport, "pkg/foo/foo.go", src), 1)
}

func TestHTTPClientTransportAllowsClientWithTransport(t *testing.T) {
	t.Parallel()
	src := `package mypackage
import "net/http"
var c = &http.Client{Transport: http.DefaultTransport}
`
	assert.Empty(t, coptest.RunNamed(t, HTTPClientTransport, "pkg/foo/foo.go", src))
}

func TestHTTPClientTransportAllowsHTTPClientPackage(t *testing.T) {
	t.Parallel()
	src := `package httpclient
import "net/http"
func NewHTTPClient() *http.Client {
	return &http.Client{}
}
`
	assert.Empty(t, coptest.RunNamed(t, HTTPClientTransport, "pkg/httpclient/client.go", src))
}

func TestHTTPClientTransportAllowsTests(t *testing.T) {
	t.Parallel()
	src := `package mypackage
import "net/http"
func TestFoo(t *testing.T) {
	_ = &http.Client{}
}
`
	assert.Empty(t, coptest.RunNamed(t, HTTPClientTransport, "pkg/foo/foo_test.go", src))
}

func TestHTTPClientTransportAllowsFakePackage(t *testing.T) {
	t.Parallel()
	src := `package fake
import "net/http"
var c = &http.Client{}
`
	assert.Empty(t, coptest.RunNamed(t, HTTPClientTransport, "pkg/fake/proxy.go", src))
}
