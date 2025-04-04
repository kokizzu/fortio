// Copyright 2020 Fortio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Tee off traffic

package fhttp // import "fortio.org/fortio/fhttp"

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"

	"fortio.org/fortio/fnet"
	"fortio.org/fortio/jrpc"
	"fortio.org/log"
)

var (
	// EnvoyRequestID is the header set by envoy and we need to propagate for distributed tracing.
	EnvoyRequestID = textproto.CanonicalMIMEHeaderKey("x-request-id")
	// TraceHeader is the single aggregated open tracing header to propagate when present.
	TraceHeader = textproto.CanonicalMIMEHeaderKey("b3")
	// TraceHeadersPrefix is the prefix for the multi header version of open zipkin.
	TraceHeadersPrefix = textproto.CanonicalMIMEHeaderKey("x-b3-")
)

// TargetConf is the structure to configure one of the multiple targets for MultiServer.
type TargetConf struct {
	Destination  string // Destination URL or base
	MirrorOrigin bool   // whether to use the incoming request as URI and data params to outgoing one (proxy like)
	//	Return       bool   // Will return the result of this target
}

// MultiServerConfig configures the MultiServer and holds the HTTP client it uses for proxying.
type MultiServerConfig struct {
	Targets []TargetConf
	Serial  bool // Serialize or parallel queries
	//	Javascript bool // return data as UI suitable
	Name   string
	client *http.Client
}

func makeMirrorRequest(baseURL string, r *http.Request, data []byte) *http.Request {
	url := baseURL + r.RequestURI
	bodyReader := io.NopCloser(bytes.NewReader(data))
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, bodyReader)
	if err != nil {
		log.Warnf("new mirror request error for %q: %v", url, err)
		return nil
	}
	// Copy all headers
	// Host header is not in Header so safe to copy
	CopyHeaders(req, r, true)
	return req
}

// CopyHeaders copies all or trace headers from `r` into `req`.
func CopyHeaders(req, r *http.Request, all bool) {
	// Copy only trace headers unless all is true.
	for k, v := range r.Header {
		if all || k == EnvoyRequestID || k == TraceHeader || strings.HasPrefix(k, TraceHeadersPrefix) {
			for _, vv := range v {
				req.Header.Add(k, vv)
			}
			log.Debugf("Adding header %q = %q", k, v)
		} else {
			log.Debugf("Skipping header %q", k)
		}
	}
	if _, ok := r.Header["User-Agent"]; !ok {
		// explicitly disable User-Agent so it's not set
		// to default value (go client lib 'feature' workaround)
		req.Header.Set("User-Agent", "")
	}
}

// MakeSimpleRequest makes a new request for url but copies trace headers from input request r.
// or all the headers if copyAllHeaders is true.
func MakeSimpleRequest(url string, r *http.Request, copyAllHeaders bool) (*http.Request, *HTTPOptions) {
	opts := CommonHTTPOptionsFromForm(r)
	var body io.Reader
	if len(opts.Payload) > 0 {
		body = bytes.NewReader(opts.Payload)
	}
	req, err := http.NewRequestWithContext(r.Context(), opts.Method(), url, body)
	if err != nil {
		log.Warnf("new request error for %q: %v", url, err)
		return nil, opts
	}
	// Copy only trace headers or all of them:
	CopyHeaders(req, r, copyAllHeaders)
	if copyAllHeaders {
		// Add the headers from the form/query args "H" arguments: (only in trusted/copy all headers mode)
		for k, v := range opts.extraHeaders {
			for _, vv := range v {
				req.Header.Add(k, vv)
			}
			log.Debugf("Header %q is now %v", k, req.Header[k])
		}
		if opts.ContentType != "" {
			req.Header.Set("Content-Type", opts.ContentType)
			log.Debugf("Setting Content-Type to %q", opts.ContentType)
		}
		// force correct content length:
		req.Header.Set("Content-Length", strconv.Itoa(len(opts.Payload)))
	} else {
		req.Header.Set(jrpc.UserAgentHeader, jrpc.UserAgent)
	}
	return req, opts
}

// TeeHandler common part between TeeSerialHandler and TeeParallelHandler.
func (mcfg *MultiServerConfig) TeeHandler(w http.ResponseWriter, r *http.Request) {
	if log.LogVerbose() {
		log.LogRequest(r, mcfg.Name)
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		log.Errf("Error reading on %v: %v", r, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.Body.Close()
	if mcfg.Serial {
		mcfg.TeeSerialHandler(w, r, data)
	} else {
		mcfg.TeeParallelHandler(w, r, data)
	}
}

func setupRequest(r *http.Request, i int, t TargetConf, data []byte) *http.Request {
	var req *http.Request
	if t.MirrorOrigin {
		req = makeMirrorRequest(t.Destination, r, data)
	} else {
		req, _ = MakeSimpleRequest(t.Destination, r, false)
	}
	if req == nil {
		// error already logged
		return nil
	}
	OnBehalfOfRequest(req, r)
	req.Header.Add("X-Fortio-Multi-Id", strconv.Itoa(i+1))
	log.LogVf("Going to %s", req.URL.String())
	return req
}

// TeeSerialHandler handles teeing off traffic in serial (one at a time) mode.
func (mcfg *MultiServerConfig) TeeSerialHandler(w http.ResponseWriter, r *http.Request, data []byte) {
	first := true
	for i, t := range mcfg.Targets {
		req := setupRequest(r, i, t, data)
		if req == nil {
			continue
		}
		url := req.URL.String()
		resp, err := mcfg.client.Do(req)
		if err != nil {
			msg := fmt.Sprintf("Error for %s: %v", url, err)
			log.Warnf(msg) //nolint:govet // we want to not duplicate the sprintf.
			if first {
				w.WriteHeader(http.StatusServiceUnavailable)
				first = false
			}
			_, _ = w.Write([]byte(msg))
			_, _ = w.Write([]byte("\n"))
			continue
		}
		if first {
			w.WriteHeader(resp.StatusCode)
			first = false
		}
		w, err := fnet.Copy(w, resp.Body)
		if err != nil {
			log.Warnf("Error copying response for %s: %v", url, err)
		}
		log.LogVf("copied %d from %s - code %d", w, url, resp.StatusCode)
		_ = resp.Body.Close()
	}
}

func singleRequest(client *http.Client, w io.Writer, req *http.Request, statusPtr *int) {
	url := req.URL.String()
	resp, err := client.Do(req)
	if err != nil {
		msg := fmt.Sprintf("Error for %s: %v", url, err)
		log.Warnf(msg) //nolint:govet // we want to not duplicate the sprintf.
		_, _ = w.Write([]byte(msg))
		_, _ = w.Write([]byte{'\n'})
		*statusPtr = -1
		return
	}
	*statusPtr = resp.StatusCode
	bw, err := fnet.Copy(w, resp.Body)
	if err != nil {
		log.Warnf("Error copying response for %s: %v", url, err)
	}
	log.LogVf("sr copied %d from %s - code %d", bw, url, resp.StatusCode)
	_ = resp.Body.Close()
}

// TeeParallelHandler handles teeing off traffic in parallel (one goroutine each) mode.
func (mcfg *MultiServerConfig) TeeParallelHandler(w http.ResponseWriter, r *http.Request, data []byte) {
	var wg sync.WaitGroup
	numTargets := len(mcfg.Targets)
	ba := make([]bytes.Buffer, numTargets)
	sa := make([]int, numTargets)
	for i := range numTargets {
		req := setupRequest(r, i, mcfg.Targets[i], data)
		if req == nil {
			continue
		}
		wg.Add(1)
		go func(client *http.Client, buffer *bytes.Buffer, request *http.Request, statusPtr *int) {
			writer := bufio.NewWriter(buffer)
			singleRequest(client, writer, request, statusPtr)
			writer.Flush()
			wg.Done()
		}(mcfg.client, &ba[i], req, &sa[i])
	}
	wg.Wait()
	// Get overall status only ok if all OK, first non ok sets status
	status := http.StatusOK
	for i := range numTargets {
		if sa[i] != http.StatusOK {
			status = sa[i]
			break
		}
	}
	if status <= 0 {
		status = http.StatusServiceUnavailable
	}
	w.WriteHeader(status)
	// Send all the data back to back
	for i := range numTargets {
		bw, err := w.Write(ba[i].Bytes())
		log.Debugf("For %d, wrote %d bytes - status %d", i, bw, sa[i])
		if err != nil {
			log.Warnf("Error writing back to %s: %v", r.RemoteAddr, err)
			break
		}
	}
}

func setClientOptions(client *http.Client, opts *HTTPOptions) {
	log.Debugf("Setting client options to %+v", opts)
	client.Timeout = opts.HTTPReqTimeOut
	tls, _ := opts.TLSConfig()
	client.Transport = &http.Transport{
		// TODO make configurable, should be fine for now for most but extreme -c values
		MaxIdleConnsPerHost: 128, // must be more than incoming parallelization; divided by number of fan out if using parallel mode
		MaxIdleConns:        256,
		// This avoids Accept-Encoding: gzip being added to outgoing requests when no encoding accept is specified
		// yet if passed by request, it will do gzip end to end. Issue #624.
		DisableCompression:  true,
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: DefaultHTTPOptions.HTTPReqTimeOut,
		TLSClientConfig:     tls,
	}
}

// CreateProxyClient HTTP client for connection reuse.
func CreateProxyClient() *http.Client {
	log.Debugf("Creating proxy client")
	client := &http.Client{}
	setClientOptions(client, DefaultHTTPOptions)
	return client
}

// MultiServer starts fan out HTTP server on the given port.
// Returns the mux and addr where the listening socket is bound.
// The port can be retrieved from it when requesting the 0 port as
// input for dynamic HTTP server.
func MultiServer(port string, cfg *MultiServerConfig) (*http.ServeMux, net.Addr) {
	hName := cfg.Name
	if hName == "" {
		hName = "Multi on " + port // port could be :0 for dynamic...
	}
	mux, addr := HTTPServer(hName, port)
	if addr == nil {
		return nil, nil // error already logged
	}
	aStr := addr.String()
	if cfg.Name == "" {
		// get actual bound port in case of :0
		cfg.Name = "Multi on " + aStr
	}
	cfg.client = CreateProxyClient()
	for i := range cfg.Targets {
		t := &cfg.Targets[i]
		if t.MirrorOrigin {
			t.Destination = strings.TrimSuffix(t.Destination, "/") // remove trailing / because we will concatenate the request URI
		}
		if !strings.HasPrefix(t.Destination, fnet.PrefixHTTPS) && !strings.HasPrefix(t.Destination, fnet.PrefixHTTP) {
			log.Infof("Assuming http:// on missing scheme for '%s'", t.Destination)
			t.Destination = fnet.PrefixHTTP + t.Destination
		}
	}
	log.Infof("Multi-server on %s running with %+v", aStr, cfg)
	mux.HandleFunc("/", cfg.TeeHandler)
	return mux, addr
}
