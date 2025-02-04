// Copyright 2023 Google LLC
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

package httptransport

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/detect"
	"cloud.google.com/go/auth/internal"
	"cloud.google.com/go/auth/internal/transport"
)

// ClientCertProvider is a function that returns a TLS client certificate to be
// used when opening TLS connections. It follows the same semantics as
// [crypto/tls.Config.GetClientCertificate].
type ClientCertProvider = func(*tls.CertificateRequestInfo) (*tls.Certificate, error)

// Options used to configure a [net/http.Client] from [NewClient].
type Options struct {
	// DisableTelemetry disables default telemetry (OpenCensus). An example
	// reason to do so would be to bind custom telemetry that overrides the
	// defaults.
	DisableTelemetry bool
	// DisableAuthentication specifies that no authentication should be used. It
	// is suitable only for testing and for accessing public resources, like
	// public Google Cloud Storage buckets.
	DisableAuthentication bool
	// Headers are extra HTTP headers that will be appended to every outgoing
	// request.
	Headers http.Header
	// Endpoint overrides the default endpoint to be used for a service.
	Endpoint string
	// APIKey specifies an API key to be used as the basis for authentication.
	// If set DetectOpts are ignored.
	APIKey string
	// TokenProvider specifies the provider used to add Authorization header to
	// all requests. If set DetectOpts are ignored.
	TokenProvider auth.TokenProvider
	// ClientCertProvider is a function that returns a TLS client certificate to
	// be used when opening TLS connections. It follows the same semantics as
	// crypto/tls.Config.GetClientCertificate.
	ClientCertProvider ClientCertProvider
	// DetectOpts configures settings for detect Application Default
	// Credentials.
	DetectOpts *detect.Options

	// InternalOptions are NOT meant to be set directly by consumers of this
	// package, they should only be set by generated client code.
	InternalOptions *InternalOptions
}

func (o *Options) validate() error {
	if o == nil {
		return errors.New("httptransport: opts required to be non-nil")
	}
	hasCreds := o.APIKey != "" ||
		o.TokenProvider != nil ||
		(o.DetectOpts != nil && len(o.DetectOpts.CredentialsJSON) > 0) ||
		(o.DetectOpts != nil && o.DetectOpts.CredentialsFile != "")
	if o.DisableAuthentication && hasCreds {
		return errors.New("httptransport: DisableAuthentication is incompatible with options that set or detect credentials")
	}
	return nil
}

// client returns the client a user set for the detect options or nil if one was
// not set.
func (o *Options) client() *http.Client {
	if o.DetectOpts != nil && o.DetectOpts.Client != nil {
		return o.DetectOpts.Client
	}
	return nil
}

func (o *Options) resolveDetectOptions() *detect.Options {
	io := o.InternalOptions
	// soft-clone these so we are not updating a ref the user holds and may reuse
	do := transport.CloneDetectOptions(o.DetectOpts)

	// If scoped JWTs are enabled user provided an aud, allow self-signed JWT.
	if (io != nil && io.EnableJWTWithScope) || do.Audience != "" {
		do.UseSelfSignedJWT = true
	}
	// Only default scopes if user did not also set an audience.
	if len(do.Scopes) == 0 && do.Audience == "" && io != nil && len(io.DefaultScopes) > 0 {
		do.Scopes = make([]string, len(io.DefaultScopes))
		copy(do.Scopes, io.DefaultScopes)
	}
	if len(do.Scopes) == 0 && do.Audience == "" && io != nil {
		do.Audience = o.InternalOptions.DefaultAudience
	}
	return do
}

// InternalOptions are only meant to be set by generated client code. These are
// not meant to be set directly by consumers of this package. Configuration in
// this type is considered EXPERIMENTAL and may be removed at any time in the
// future without warning.
type InternalOptions struct {
	// EnableJWTWithScope specifies if scope can be used with self-signed JWT.
	EnableJWTWithScope bool
	// DefaultAudience specifies a default audience to be used as the audience
	// field ("aud") for the JWT token authentication.
	DefaultAudience string
	// DefaultEndpoint specifies the default endpoint.
	DefaultEndpoint string
	// DefaultMTLSEndpoint specifies the default mTLS endpoint.
	DefaultMTLSEndpoint string
	// DefaultScopes specifies the default OAuth2 scopes to be used for a
	// service.
	DefaultScopes []string
}

// AddAuthorizationMiddleware adds a middleware to the provided client's
// transport that sets the Authorization header with the value produced by the
// provided [cloud.google.com/go/auth.TokenProvider]. An error is returned only
// if client or tp is nil.
func AddAuthorizationMiddleware(client *http.Client, tp auth.TokenProvider) error {
	if client == nil || tp == nil {
		return fmt.Errorf("httptransport: client and tp must not be nil")
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	}
	client.Transport = &authTransport{
		provider: auth.NewCachedTokenProvider(tp, nil),
		base:     base,
	}
	return nil
}

// NewClient returns a [net/http.Client] that can be used to communicate with a
// Google cloud service, configured with the provided [Options]. It
// automatically appends Authorization headers to all outgoing requests.
func NewClient(opts *Options) (*http.Client, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	// TODO(codyoss): re-add in a future PR

	// tOpts := &transport.Options{
	// 	Endpoint:           opts.Endpoint,
	// 	ClientCertProvider: opts.ClientCertProvider,
	// 	Client:             opts.client(),
	// }
	// if io := opts.InternalOptions; io != nil {
	// 	tOpts.DefaultEndpoint = io.DefaultEndpoint
	// 	tOpts.DefaultMTLSEndpoint = io.DefaultMTLSEndpoint
	// }
	// clientCertProvider, dialTLSContext, err := transport.GetHTTPTransportConfig(tOpts)
	// if err != nil {
	// 	return nil, err
	// }
	trans, err := newTransport(defaultBaseTransport(nil), opts)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: trans,
	}, nil
}

// SetAuthHeader uses the provided token to set the Authorization header on a
// request. If the token.Type is empty, the type is assumed to be Bearer.
func SetAuthHeader(token *auth.Token, req *http.Request) {
	typ := token.Type
	if typ == "" {
		typ = internal.TokenTypeBearer
	}
	req.Header.Set("Authorization", typ+" "+token.Value)
}
