// Copyright 2022 Fortio Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package jrpc_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"fortio.org/assert"
	"fortio.org/fortio/fhttp"
	"fortio.org/fortio/jrpc"
	"fortio.org/sets"
)

func TestDebugSummary(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"12345678", "12345678"},
		{"123456789", "123456789"},
		{"1234567890", "1234567890"},
		{"12345678901", "12345678901"},
		{"123456789012", "12: 1234...9012"},
		{"1234567890123", "13: 1234...0123"},
		{"12345678901234", "14: 1234...1234"},
		{"A\r\000\001\x80\nB", `A\r\x00\x01\x80\nB`},                   // escaping
		{"A\r\000Xyyyyyyyyy\001\x80\nB", `17: A\r\x00X...\x01\x80\nB`}, // escaping
	}
	for _, tst := range tests {
		if actual := jrpc.DebugSummary([]byte(tst.input), 8); actual != tst.expected {
			t.Errorf("Got '%s', expected '%s' for DebugSummary(%q)", actual, tst.expected, tst.input)
		}
	}
}

// Rest is also tested in rapi/restHandler_tests.go, but that doesn't count for coverage

type Request struct {
	SomeInt    int
	SomeString []string
}

type Response struct {
	jrpc.ServerReply
	InputInt            int
	ConcatenatedStrings string
}

//nolint:funlen,gocognit,maintidx // lots of tests using same server setup
func TestJPRC(t *testing.T) {
	prev := jrpc.SetCallTimeout(5 * time.Second)
	if prev != 60*time.Second {
		t.Errorf("Expected default call timeout to be 60 seconds, got %v", prev)
	}
	mux, addr := fhttp.HTTPServer("test1", "0")
	port := addr.(*net.TCPAddr).Port
	var bad chan struct{}
	mux.HandleFunc("/test-api", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			err := jrpc.ReplyError(w, "should be a POST", nil)
			if err != nil {
				t.Errorf("Error in replying error: %v", err)
			}
			return
		}
		req, err := jrpc.HandleCall[Request](w, r)
		if err != nil {
			err = jrpc.ReplyError(w, "request error", err)
			if err != nil {
				t.Errorf("Error in replying error: %v", err)
			}
			return
		}
		if req.SomeInt == -7 {
			jrpc.ReplyNoPayload(w, 777)
			return
		}
		resp := Response{}
		if req.SomeInt == -8 {
			resp.Error = true
			resp.Message = "simulated server error"
			jrpc.ReplyServerError(w, &resp)
			return
		}
		if req.SomeInt == -9 {
			// simulate a bad reply, invalid JSON
			w.WriteHeader(747)
			w.Write([]byte(`{bad}`))
			return
		}
		if req.SomeInt == -10 {
			// simulate a bad reply, invalid JSON, but HTTP OK status code
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{notjson}`))
			return
		}
		if req.SomeInt == -11 {
			// server error using unserializable struct
			err = jrpc.Reply(w, 200, &bad)
			if err == nil {
				t.Errorf("Expected bad serialization error")
			}
			return
		}
		resp.Message = "works"
		resp.InputInt = req.SomeInt
		// inefficient but this is just to test
		for _, s := range req.SomeString {
			resp.ConcatenatedStrings += s
		}
		jrpc.ReplyOk(w, &resp)
	})
	url := fmt.Sprintf("http://localhost:%d/test-api", port)
	req := Request{42, []string{"ab", "cd"}}
	res, err := jrpc.CallURL[Response](url, &req)
	if err != nil {
		t.Errorf("failed Call: %v", err)
	}
	if res.Error {
		t.Errorf("response unexpectedly marked as failed: %+v", res)
	}
	if res.InputInt != 42 {
		t.Errorf("response doesn't contain expected int: %+v", res)
	}
	if res.ConcatenatedStrings != "abcd" {
		t.Errorf("response doesn't contain expected string: %+v", res)
	}
	// OK case: empty POST
	dest := &jrpc.Destination{
		URL:    url,
		Method: http.MethodPost, // force post (default is get when no payload)
	}
	res, err = jrpc.Fetch[Response](dest, []byte{})
	if err != nil {
		t.Errorf("failed Fetch with POST and empty body: %v", err)
	}
	if res.Error {
		t.Errorf("response unexpectedly marked as failed: %+v", res)
	}
	// Error cases
	// Empty request, using FetchBytes()
	code, bytes, err := jrpc.FetchBytes(jrpc.NewDestination(url))
	if err != nil {
		t.Errorf("failed Fetch: %v - %s", err, jrpc.DebugSummary(bytes, 256))
	}
	if code != http.StatusBadRequest {
		t.Errorf("expected status code 400, got %d - %s", code, jrpc.DebugSummary(bytes, 256))
	}
	res, err = jrpc.Deserialize[Response](bytes)
	if err != nil {
		t.Errorf("failed Deserialize: %v - %s", err, jrpc.DebugSummary(bytes, 256))
	}
	if !res.Error {
		t.Errorf("response unexpectedly marked as not failed: %+v", res)
	}
	if res.Message != "should be a POST" {
		t.Errorf("response doesn't contain expected message: %+v", res)
	}
	// bad url
	badURL := "http://doesnotexist.fortio.org/"
	_, err = jrpc.CallURL[Response](badURL, &req)
	if err == nil {
		t.Errorf("expected error for bad url")
	}
	var de *net.DNSError
	if !errors.As(err, &de) {
		t.Errorf("expected dns error, got %v", err)
	}
	expected := "lookup doesnotexist.fortio.org"
	if de != nil && !strings.HasPrefix(de.Error(), expected) {
		t.Errorf("expected dns error to start with %q, got %q", expected, de.Error())
	}
	errReply, err := jrpc.Fetch[Response](jrpc.NewDestination(url), []byte(`{foo: missing-quotes}`))
	if err == nil {
		t.Errorf("expected error, got nil and %v", res)
	}
	var fe *jrpc.FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("error supposed to be FetchError %v: %v", res, err)
	}
	if fe.Code != http.StatusBadRequest {
		t.Errorf("expected status code %d, got %d", http.StatusBadRequest, fe.Code)
	}
	if errReply == nil {
		t.Fatalf("expected error reply, got nil")
	}
	if errReply.Message != "request error" {
		t.Errorf("unexpected Message in %+v", errReply.Message)
	}
	expected = "invalid character 'f' looking for beginning of object key string"
	if errReply.Exception != expected {
		t.Errorf("expected Exception in body to be %q, got %+v", expected, errReply)
	}
	// bad JSON response, using GetURL()
	errReply, err = jrpc.GetURL[Response](url)
	if err == nil {
		t.Errorf("expected error %v", errReply)
	}
	if code != http.StatusBadRequest {
		t.Errorf("expected status code 400, got %d - %v - %v", code, err, errReply)
	}
	if !errReply.Error {
		t.Errorf("response unexpectedly marked as not failed: %+v", res)
	}
	// trigger empty reply
	req.SomeInt = -7
	res, err = jrpc.CallURL[Response](url, &req)
	if err == nil {
		t.Errorf("error expected %v: %v", res, err)
	}
	if !errors.As(err, &fe) {
		t.Errorf("error supposed to be FetchError %v: %v", res, err)
	}
	if fe != nil && fe.Code != 777 {
		t.Errorf("error code expected for -7 to be 777: %v: %v", res, err)
	}
	// trigger server error
	req.SomeInt = -8
	res, err = jrpc.CallURL[Response](url, &req)
	if err == nil {
		t.Errorf("error expected %v: %v", res, err)
	}
	fe = nil
	if !errors.As(err, &fe) {
		t.Errorf("error supposed to be FetchError %v: %v", res, err)
	}
	if fe != nil && fe.Code != http.StatusServiceUnavailable {
		t.Errorf("error code expected for -8 to be 503: %v: %v", res, err)
	}
	if !res.Error {
		t.Errorf("response supposed to be marked as failed: %+v", res)
	}
	if res.Message != "simulated server error" {
		t.Errorf("didn't get the error message expected for -8: %v: %v", res, err)
	}
	// trigger bad JSON response - and non-OK HTTP status code
	req.SomeInt = -9
	res, err = jrpc.CallURL[Response](url, &req)
	if err == nil {
		t.Errorf("error expected %v: %v", res, err)
	}
	fe = nil
	if !errors.As(err, &fe) {
		t.Errorf("error supposed to be FetchError %v: %v", res, err)
	}
	if fe != nil && fe.Code != 747 {
		t.Errorf("error code expected for -9 to be 747: %v: %v", res, err)
	}
	unwrap := fe.Unwrap()
	if unwrap == nil {
		t.Errorf("unwrapped error is nil: %+v", fe)
	}
	expected = "invalid character 'b' looking for beginning of object key string"
	if unwrap.Error() != expected {
		t.Errorf("unwrapped error expected to be %q, got %v", expected, unwrap.Error())
	}
	expected = "non ok http result and deserialization error, code 747: " + expected + " (raw reply: {bad})"
	if err.Error() != expected {
		t.Errorf("error string expected %q, got %q", expected, err.Error())
	}
	// trigger bad JSON response - and HTTP OK status code
	req.SomeInt = -10
	res, err = jrpc.CallURL[Response](url, &req)
	if err == nil {
		t.Errorf("error expected %v: %v", res, err)
	}
	expected = "invalid character 'n' looking for beginning of object key string"
	if err.Error() != expected {
		t.Errorf("error string expected %q, got %q", expected, err.Error())
	}
	// trigger reply bad serialization
	req.SomeInt = -11
	res, err = jrpc.CallURL[Response](url, &req)
	if err == nil {
		t.Errorf("error expected %v", res)
	}
	expected = "non ok http result, code 500: <nil> (raw reply: )"
	if err.Error() != expected {
		t.Errorf("error string expected %q, got %q, %+v", expected, err.Error(), res)
	}
	// Unserializable client side
	res, err = jrpc.CallURL[Response](url, &bad)
	if err == nil {
		t.Errorf("error expected %v", res)
	}
	expected = "json: unsupported type: chan struct {}"
	if err.Error() != expected {
		t.Errorf("error string expected %q, got %q, %+v", expected, err.Error(), res)
	}
}

func TestJPRCHeaders(t *testing.T) {
	mux, addr := fhttp.HTTPServer("test2", "0")
	port := addr.(*net.TCPAddr).Port
	mux.HandleFunc("/test-headers", func(w http.ResponseWriter, r *http.Request) {
		// Send back the headers
		jrpc.ReplyOk(w, &r.Header)
	})
	url := fmt.Sprintf("http://localhost:%d/test-headers", port)
	inp := make(http.Header)
	inp.Set("Test1", "ValT1.1")
	inp.Add("Test1", "ValT1.2")
	inp.Set("Test2", "ValT2")
	jrpc.SetHeaderIfMissing(inp, "Test2", "ShouldNotSet") // test along the way
	gotFirstByte := false
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			gotFirstByte = true
		},
	}
	dest := &jrpc.Destination{
		URL:         url,
		Headers:     &inp,
		ClientTrace: trace,
	}
	res, err := jrpc.Get[http.Header](dest)
	if err != nil {
		t.Errorf("failed Call: %v", err)
	}
	// order etc is preserved, keys are not case-sensitive (kinda tests go HTTP API too)
	//nolint:canonicalheader // we want to test case insensitivity
	assert.Equal(t, res.Values("test1"), []string{"ValT1.1", "ValT1.2"}, "Expecting echoed back Test1 multi valued header")
	//nolint:canonicalheader // we want to test case insensitivity
	assert.CheckEquals(t, res.Get("tESt2"), "ValT2", "Expecting echoed back Test2 header")
	if !gotFirstByte {
		t.Errorf("expected trace callback to have been called")
	}
}

type ErrReader struct{}

const ErrReaderMessage = "simulated IO error"

func (ErrReader) Read(_ []byte) (n int, err error) {
	return 0, errors.New(ErrReaderMessage)
}

func TestHandleCallError(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/", ErrReader{})
	_, err := jrpc.ProcessRequest[jrpc.ServerReply](r)
	if err == nil {
		t.Errorf("expected error, got nil")
	}
	if err.Error() != ErrReaderMessage {
		t.Errorf("expected error %q, got %q", ErrReaderMessage, err.Error())
	}
}

func TestSendBadURL(t *testing.T) {
	badURL := "bad\001url" // something caught in NewRequest
	_, _, err := jrpc.FetchURL(badURL)
	if err == nil {
		t.Errorf("expected error, got nil")
	}
	expected := `parse "bad\x01url": net/url: invalid control character in URL`
	if err.Error() != expected {
		t.Errorf("expected error %q, got %q", expected, err.Error())
	}
}

type TestError struct {
	Message string
	Extra   int
}

func (e TestError) Error() string {
	return e.Message
}

func TestSerializeServerReply(t *testing.T) {
	o := &jrpc.ServerReply{}
	bytes, err := jrpc.Serialize(o)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	str := string(bytes)
	expected := `{}`
	if str != expected {
		t.Errorf("expected %s, got %s", expected, str)
	}
	o = jrpc.NewErrorReply("a message", nil)
	bytes, err = jrpc.Serialize(o)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	str = string(bytes)
	expected = `{"error":true,"message":"a message"}`
	if str != expected {
		t.Errorf("expected %s, got %s", expected, str)
	}
	e := TestError{Message: "an error", Extra: 42}
	o = jrpc.NewErrorReply("a message", &e)
	bytes, err = jrpc.Serialize(o)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	str = string(bytes)
	expected = `{"error":true,"message":"a message","exception":"an error","exceptionDetails":{"Message":"an error","Extra":42}}`
	if str != expected {
		t.Errorf("expected %s, got %s", expected, str)
	}
}

// Testing slices

type SliceRequest struct {
	HowMany int
}

type SliceOneResponse struct {
	Index int
	Data  string
}

func TestJPRCSlices(t *testing.T) {
	mux, addr := fhttp.HTTPServer("test3", "0")
	port := addr.(*net.TCPAddr).Port
	mux.HandleFunc("/test-api-array", func(w http.ResponseWriter, r *http.Request) {
		req, err := jrpc.ProcessRequest[SliceRequest](r)
		if err != nil {
			err = jrpc.ReplyError(w, "request error", err)
			if err != nil {
				t.Errorf("Error in replying error: %v", err)
			}
			return
		}
		n := req.HowMany
		if n < 0 {
			jrpc.ReplyError(w, "invalid negative count", nil)
			return
		}
		if r.FormValue("error") != "" {
			jrpc.ReplyError(w, "error requested", nil)
			return
		}
		if n == 0 {
			n = 42 // for testing of GetArray
		}
		resp := make([]SliceOneResponse, n)
		for i := range n {
			resp[i] = SliceOneResponse{
				Index: i,
				Data:  fmt.Sprintf("data %d", i),
			}
		}
		jrpc.ReplyOk(w, &resp)
	})
	url := fmt.Sprintf("http://localhost:%d/test-api-array", port)
	req := SliceRequest{10}
	res, err := jrpc.CallURL[[]SliceOneResponse](url, &req)
	if err != nil {
		t.Errorf("failed Call: %v", err)
	}
	if res == nil {
		t.Errorf("nil response")
		return
	}
	slice := *res
	if len(slice) != 10 {
		t.Errorf("expected 10 results, got %d", len(slice))
	}
	for i := range slice {
		el := slice[i]
		if el.Index != i {
			t.Errorf("expected index %d, got %d", i, el.Index)
		}
		if el.Data != fmt.Sprintf("data %d", i) {
			t.Errorf("expected data %d, got %s", i, el.Data)
		}
	}
	slice, err = jrpc.GetArray[SliceOneResponse](jrpc.NewDestination(url))
	if err != nil {
		t.Errorf("failed GetArray: %v", err)
	}
	if len(slice) != 42 {
		t.Errorf("expected 42 results, got %d", len(slice))
	}
	for i := range slice {
		el := slice[i]
		if el.Index != i {
			t.Errorf("expected index %d, got %d", i, el.Index)
		}
		if el.Data != fmt.Sprintf("data %d", i) {
			t.Errorf("expected data %d, got %s", i, el.Data)
		}
	}
	// Empty slice/error
	slice, err = jrpc.GetArray[SliceOneResponse](jrpc.NewDestination(url + "?error=true"))
	if err == nil {
		t.Errorf("expected error, got nil")
	}
	if slice != nil {
		t.Errorf("expected nil slice, got %v", slice)
	}
}

func TestContext(t *testing.T) {
	dest := jrpc.NewDestination("http://localhost:1234")
	ctx := dest.GetContext()
	if ctx == nil {
		t.Errorf("expected non-nil context")
	}
	if ctx != context.Background() {
		t.Errorf("expected context.Background(), got %v", ctx)
	}
	if dest.Context != nil {
		t.Errorf("expected Context inside struct to remain nil")
	}
	newCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dest.Context = newCtx
	ctx = dest.GetContext()
	if ctx != newCtx {
		t.Errorf("expected newCtx, got %v", ctx)
	}
}

// Semi copied from fhttp/https_server_test.go - to avoid circular dependencies
// TODO move these path to shared test package.
var (
	// Generated from "make cert".
	svrCrt     = "../cert-tmp/server.crt"
	svrKey     = "../cert-tmp/server.key"
	tlsOptions = &fhttp.TLSOptions{Cert: svrCrt, Key: svrKey}
)

func TestTLS(t *testing.T) {
	// log.SetLogLevel(log.Debug)
	m, a := fhttp.ServeTLS("0", "", tlsOptions)
	if m == nil || a == nil {
		t.Errorf("Failed to create server %v %v", m, a)
	}
	url := fmt.Sprintf("https://localhost:%d/", a.(*net.TCPAddr).Port)
	d1 := &jrpc.Destination{URL: url}
	_, err := jrpc.Get[struct{}](d1)
	if err == nil {
		t.Errorf("expected error, got nil - should have complained about TLS")
	}
	d1.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	_, err = jrpc.Get[struct{}](d1)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	// If we change the client, we should be back to errors again
	d1.Client = &http.Client{}
	_, err = jrpc.Get[struct{}](d1)
	if err == nil {
		t.Errorf("expected error, got nil - should have complained about TLS when passing a new client")
	}
	d1.Client = nil
	d1.URL += "?status=417" // tea pot
	_, err = jrpc.Get[struct{}](d1)
	if err == nil {
		t.Errorf("expected error, got nil - should have complained about return code 417")
	}
	d1.OkCodes = sets.New(233, 417)
	_, err = jrpc.Get[struct{}](d1)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	// 200 should error again
	d1.URL = url
	_, err = jrpc.Get[struct{}](d1)
	if err == nil {
		t.Errorf("expected error, got nil - should have complained about return code 200")
	}
}
