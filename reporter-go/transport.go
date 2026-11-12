package reporter

import (
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/go-fynx/reporter/reporter-go/proto/reporter/v1/reporterv1connect"
)

// newSender returns a gRPC client for cfg.Endpoint. gRPC needs HTTP/2: over
// TLS for https:// endpoints, and unencrypted (h2c) for http:// ones.
func newSender(cfg Configuration) (sender, error) {
	rt := cfg.Transport
	if rt == nil {
		protocols := new(http.Protocols)
		if isPlaintext(cfg.Endpoint) {
			protocols.SetUnencryptedHTTP2(true)
		} else {
			protocols.SetHTTP2(true)
		}
		rt = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     90 * time.Second,
			Protocols:           protocols,
		}
	}
	httpClient := &http.Client{Transport: rt}
	baseURL := strings.TrimRight(cfg.Endpoint, "/")
	return reporterv1connect.NewReporterServiceClient(
		connect.NewClient(connecthttp.NewTransport(httpClient, baseURL, connecthttp.WithGRPC())),
	), nil
}

func isPlaintext(endpoint string) bool {
	return strings.HasPrefix(strings.ToLower(endpoint), "http://")
}
