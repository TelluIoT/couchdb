// Package chttp provides a minimal HTTP driver backend for communicating with
// CouchDB servers.
package chttp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"github.com/go-kivik/kivik"
	"github.com/go-kivik/kivik/errors"
)

const (
	typeJSON = "application/json"
)

// The default UserAgent values
const (
	UserAgent = "Kivik chttp"
	Version   = "2.0.0-prerelease"
)

// Client represents a client connection. It embeds an *http.Client
type Client struct {
	// UserAgent is used to set the User-Agent header product string. If unset,
	// a default value is used.
	UserAgent string
	// UserAgentVersion is used to set the User-Agent product version. If unset,
	// the version of this module is used.
	UserAgentVersion string

	*http.Client

	rawDSN string
	dsn    *url.URL
	auth   Authenticator
}

// New returns a connection to a remote CouchDB server. If credentials are
// included in the URL, requests will be authenticated using Cookie Auth. To
// use HTTP BasicAuth or some other authentication mechanism, do not specify
// credentials in the URL, and instead call the Auth() method later.
func New(dsn string) (*Client, error) {
	dsnURL, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	user := dsnURL.User
	dsnURL.User = nil
	c := &Client{
		Client: &http.Client{},
		dsn:    dsnURL,
		rawDSN: dsn,
	}
	if user != nil {
		password, _ := user.Password()
		err := c.Auth(&CookieAuth{
			Username: user.Username(),
			Password: password,
		})
		if err != nil {
			return nil, err
		}
	}
	return c, nil
}

func parseDSN(dsn string) (*url.URL, error) {
	if dsn == "" {
		return nil, &HTTPError{Code: kivik.StatusBadAPICall, Reason: "no URL specified", exitStatus: ExitFailedToInitialize}
	}
	if !strings.HasPrefix(dsn, "http://") && !strings.HasPrefix(dsn, "https://") {
		dsn = "http://" + dsn
	}
	dsnURL, err := url.Parse(dsn)
	if err != nil {
		return nil, fullError(kivik.StatusBadAPICall, ExitStatusURLMalformed, err)
	}
	if dsnURL.Path == "" {
		dsnURL.Path = "/"
	}
	return dsnURL, nil
}

// DSN returns the unparsed DSN used to connect.
func (c *Client) DSN() string {
	return c.rawDSN
}

// Auth authenticates using the provided Authenticator.
func (c *Client) Auth(a Authenticator) error {
	if c.auth != nil {
		return errors.New("auth already set")
	}
	if err := a.Authenticate(c); err != nil {
		return err
	}
	c.auth = a
	return nil
}

// Options are optional parameters which may be sent with a request.
type Options struct {
	// Accept sets the request's Accept header. Defaults to "application/json".
	// To specify any, use "*/*".
	Accept string

	// ContentType sets the requests's Content-Type header. Defaults to "application/json".
	ContentType string

	// Body sets the body of the request.
	Body io.ReadCloser

	// JSON is an arbitrary data type which is marshaled to the request's body.
	// It an error to set both Body and JSON on the same request. When this is
	// set, ContentType is unconditionally set to 'application/json'. Note that
	// for large JSON payloads, it can be beneficial to do your own JSON stream
	// encoding, so that the request can be live on the wire during JSON
	// encoding.
	JSON interface{}

	// FullCommit adds the X-Couch-Full-Commit: true header to requests
	FullCommit bool

	// IfNoneMatch adds the If-None-Match header. The value will be quoted if
	// it is not already.
	IfNoneMatch string

	// Destination is the target ID for COPY
	Destination string

	// Query is appended to the exiting url, if present. If the passed url
	// already contains query parameters, the values in Query are appended.
	// No merging takes place.
	Query url.Values
}

// Response represents a response from a CouchDB server.
type Response struct {
	*http.Response

	// ContentType is the base content type, parsed from the response headers.
	ContentType string
}

// DecodeJSON unmarshals the response body into i. This method consumes and
// closes the response body.
func DecodeJSON(r *http.Response, i interface{}) error {
	defer r.Body.Close() // nolint: errcheck
	err := json.NewDecoder(r.Body).Decode(i)
	switch err.(type) {
	case *json.SyntaxError, *json.UnmarshalFieldError, *json.UnmarshalTypeError:
		return errors.WrapStatus(kivik.StatusBadResponse, err)
	default:
		return errors.WrapStatus(kivik.StatusNetworkError, err)
	}
}

// DoJSON combines DoReq() and, ResponseError(), and (*Response).DecodeJSON(), and
// closes the response body.
func (c *Client) DoJSON(ctx context.Context, method, path string, opts *Options, i interface{}) (*http.Response, error) {
	res, err := c.DoReq(ctx, method, path, opts)
	if err != nil {
		return res, err
	}
	if err = ResponseError(res); err != nil {
		return res, err
	}
	err = DecodeJSON(res, i)
	return res, err
}

// NewRequest returns a new *http.Request to the CouchDB server, and the
// specified path. The host, schema, etc, of the specified path are ignored.
func (c *Client) NewRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	fullPath := path
	if cPath := strings.TrimSuffix(c.dsn.Path, "/"); cPath != "" {
		fullPath = cPath + "/" + strings.TrimPrefix(path, "/")
	}
	reqPath, err := url.Parse(fullPath)
	if err != nil {
		return nil, fullError(kivik.StatusBadAPICall, ExitStatusURLMalformed, err)
	}
	u := *c.dsn // Make a copy
	u.Path = reqPath.Path
	u.RawQuery = reqPath.RawQuery
	req, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, errors.WrapStatus(kivik.StatusBadAPICall, err)
	}
	req.Header.Add("User-Agent", c.userAgent())
	return req.WithContext(ctx), nil
}

// DoReq does an HTTP request. An error is returned only if there was an error
// processing the request. In particular, an error status code, such as 400
// or 500, does _not_ cause an error to be returned.
func (c *Client) DoReq(ctx context.Context, method, path string, opts *Options) (*http.Response, error) {
	if method == "" {
		return nil, errors.Status(kivik.StatusBadAPICall, "chttp: method required")
	}
	var body io.Reader
	if opts != nil {
		if opts.Body != nil {
			body = opts.Body
			defer opts.Body.Close() // nolint: errcheck
		}
	}
	req, err := c.NewRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	fixPath(req, path)
	setHeaders(req, opts)
	setQuery(req, opts)

	trace := ContextClientTrace(ctx)
	if trace != nil {
		trace.httpRequest(req)
		trace.httpRequestBody(req)
	}

	response, err := c.Do(req)
	if trace != nil {
		trace.httpResponse(response)
		trace.httpResponseBody(response)
	}
	return response, netError(err)
}

func netError(err error) error {
	if err == nil {
		return nil
	}
	if urlErr, ok := err.(*url.Error); ok {
		// If this error was generated by EncodeBody, it may have an emedded
		// status code (!= 500), which we should honor.
		status := kivik.StatusCode(urlErr.Err)
		if status == kivik.StatusInternalServerError {
			status = kivik.StatusNetworkError
		}
		return fullError(status, curlStatus(err), err)
	}
	if status := kivik.StatusCode(err); status != kivik.StatusInternalServerError {
		return err
	}
	return fullError(kivik.StatusNetworkError, ExitUnknownFailure, err)
}

var tooManyRecirectsRE = regexp.MustCompile(`stopped after \d+ redirect`)

func curlStatus(err error) int {
	if urlErr, ok := err.(*url.Error); ok {
		// Timeout error
		if urlErr.Timeout() {
			return ExitOperationTimeout
		}
		// Host lookup failure
		if opErr, ok := urlErr.Err.(*net.OpError); ok {
			if _, ok := opErr.Err.(*net.DNSError); ok {
				return ExitHostNotResolved
			}
			if scErr, ok := opErr.Err.(*os.SyscallError); ok {
				if errno, ok := scErr.Err.(syscall.Errno); ok {
					if errno == syscall.ECONNREFUSED {
						return ExitFailedToConnect
					}
				}
			}
		}

		if tooManyRecirectsRE.MatchString(urlErr.Err.Error()) {
			return ExitTooManyRedirects
		}
	}
	return 0
}

// fixPath sets the request's URL.RawPath to work with escaped characters in
// paths.
func fixPath(req *http.Request, path string) {
	// Remove any query parameters
	parts := strings.SplitN(path, "?", 2)
	req.URL.RawPath = "/" + strings.TrimPrefix(parts[0], "/")
}

// EncodeBody JSON encodes i to an io.ReadCloser. If an encoding error
// occurs, it will be returned on the next read.
func EncodeBody(i interface{}) io.ReadCloser {
	done := make(chan struct{})
	r, w := io.Pipe()
	go func() {
		defer close(done)
		var err error
		switch t := i.(type) {
		case []byte:
			_, err = w.Write(t)
		case json.RawMessage: // Only needed for Go 1.7
			_, err = w.Write(t)
		case string:
			_, err = w.Write([]byte(t))
		default:
			err = json.NewEncoder(w).Encode(i)
			switch err.(type) {
			case *json.MarshalerError, *json.UnsupportedTypeError, *json.UnsupportedValueError:
				err = errors.WrapStatus(kivik.StatusBadAPICall, err)
			}
		}
		_ = w.CloseWithError(err)
	}()
	return &ebReader{
		ReadCloser: r,
		done:       done,
	}
}

type ebReader struct {
	io.ReadCloser
	done <-chan struct{}
}

var _ io.ReadCloser = &ebReader{}

func (r *ebReader) Close() error {
	err := r.ReadCloser.Close()
	<-r.done
	return err
}

func setHeaders(req *http.Request, opts *Options) {
	accept := typeJSON
	contentType := typeJSON
	if opts != nil {
		if opts.Accept != "" {
			accept = opts.Accept
		}
		if opts.ContentType != "" {
			contentType = opts.ContentType
		}
		if opts.FullCommit {
			req.Header.Add("X-Couch-Full-Commit", "true")
		}
		if opts.Destination != "" {
			req.Header.Add("Destination", opts.Destination)
		}
		if opts.IfNoneMatch != "" {
			inm := "\"" + strings.Trim(opts.IfNoneMatch, "\"") + "\""
			req.Header.Set("If-None-Match", inm)
		}
	}
	req.Header.Add("Accept", accept)
	req.Header.Add("Content-Type", contentType)
}

func setQuery(req *http.Request, opts *Options) {
	if opts == nil || len(opts.Query) == 0 {
		return
	}
	if req.URL.RawQuery == "" {
		req.URL.RawQuery = opts.Query.Encode()
		return
	}
	req.URL.RawQuery = strings.Join([]string{req.URL.RawQuery, opts.Query.Encode()}, "&")
}

// DoError is the same as DoReq(), followed by checking the response error. This
// method is meant for cases where the only information you need from the
// response is the status code. It unconditionally closes the response body.
func (c *Client) DoError(ctx context.Context, method, path string, opts *Options) (*http.Response, error) {
	res, err := c.DoReq(ctx, method, path, opts)
	if err != nil {
		return res, err
	}
	defer res.Body.Close() // nolint: errcheck
	err = ResponseError(res)
	return res, err
}

// ETag returns the unquoted ETag value, and a bool indicating whether it was
// found.
func ETag(resp *http.Response) (string, bool) {
	if resp == nil {
		return "", false
	}
	etag, ok := resp.Header["Etag"]
	if !ok {
		etag, ok = resp.Header["ETag"]
	}
	if !ok {
		return "", false
	}
	return strings.Trim(etag[0], `"`), ok
}

// GetRev extracts the revision from the response's Etag header
func GetRev(resp *http.Response) (rev string, err error) {
	if err = ResponseError(resp); err != nil {
		return "", err
	}
	rev, ok := ETag(resp)
	if !ok {
		return "", errors.New("no ETag header found")
	}
	return rev, nil
}

type exitStatuser interface {
	ExitStatus() int
}

// ExitStatus returns the curl exit status embedded in the error, or 1 (unknown
// error), if there was no specified exit status.  If err is nil, ExitStatus
// returns 0.
func ExitStatus(err error) int {
	if err == nil {
		return 0
	}
	if statuser, ok := err.(exitStatuser); ok {
		return statuser.ExitStatus()
	}
	return 0
}

func (c *Client) userAgent() string {
	ua := c.UserAgent
	uav := c.UserAgentVersion
	if ua == "" {
		ua = UserAgent
	}
	if uav == "" {
		uav = Version
	}
	return fmt.Sprintf("%s/%s (Language=%s; Platform=%s/%s)",
		ua, uav, runtime.Version(), runtime.GOARCH, runtime.GOOS)
}
