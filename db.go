package couchdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"strings"

	"github.com/go-kivik/couchdb/chttp"
	"github.com/go-kivik/kivik"
	"github.com/go-kivik/kivik/driver"
	"github.com/go-kivik/kivik/errors"
)

type db struct {
	*client
	dbName string
}

var _ driver.DB = &db{}
var _ driver.MetaGetter = &db{}
var _ driver.AttachmentMetaGetter = &db{}

func (d *db) path(path string, query url.Values) string {
	url, _ := url.Parse(d.dbName + "/" + strings.TrimPrefix(path, "/"))
	if query != nil {
		url.RawQuery = query.Encode()
	}
	return url.String()
}

func optionsToParams(opts ...map[string]interface{}) (url.Values, error) {
	params := url.Values{}
	for _, optsSet := range opts {
		for key, i := range optsSet {
			var values []string
			switch v := i.(type) {
			case string:
				values = []string{v}
			case []string:
				values = v
			case bool:
				values = []string{fmt.Sprintf("%t", v)}
			case int, uint, uint8, uint16, uint32, uint64, int8, int16, int32, int64:
				values = []string{fmt.Sprintf("%d", v)}
			default:
				return nil, errors.Statusf(kivik.StatusBadRequest, "kivik: invalid type %T for options", i)
			}
			for _, value := range values {
				params.Add(key, value)
			}
		}
	}
	return params, nil
}

// rowsQuery performs a query that returns a rows iterator.
func (d *db) rowsQuery(ctx context.Context, path string, opts map[string]interface{}) (driver.Rows, error) {
	options, err := optionsToParams(opts)
	if err != nil {
		return nil, err
	}
	resp, err := d.Client.DoReq(ctx, kivik.MethodGet, d.path(path, options), nil)
	if err != nil {
		return nil, err
	}
	if err = chttp.ResponseError(resp); err != nil {
		return nil, err
	}
	return newRows(resp.Body), nil
}

// AllDocs returns all of the documents in the database.
func (d *db) AllDocs(ctx context.Context, opts map[string]interface{}) (driver.Rows, error) {
	return d.rowsQuery(ctx, "_all_docs", opts)
}

// Query queries a view.
func (d *db) Query(ctx context.Context, ddoc, view string, opts map[string]interface{}) (driver.Rows, error) {
	return d.rowsQuery(ctx, fmt.Sprintf("_design/%s/_view/%s", chttp.EncodeDocID(ddoc), chttp.EncodeDocID(view)), opts)
}

// Get fetches the requested document.
func (d *db) Get(ctx context.Context, docID string, options map[string]interface{}) (*driver.Document, error) {
	resp, rev, err := d.get(ctx, http.MethodGet, docID, options)
	if err != nil {
		return nil, err
	}
	return &driver.Document{
		Rev:           rev,
		ContentLength: resp.ContentLength,
		Body:          resp.Body,
	}, nil
}

// Rev returns the most current rev of the requested document.
func (d *db) GetMeta(ctx context.Context, docID string, options map[string]interface{}) (size int64, rev string, err error) {
	resp, rev, err := d.get(ctx, http.MethodHead, docID, options)
	if err != nil {
		return 0, "", err
	}
	return resp.ContentLength, rev, err
}

func (d *db) get(ctx context.Context, method string, docID string, options map[string]interface{}) (*http.Response, string, error) {
	if docID == "" {
		return nil, "", missingArg("docID")
	}

	inm, err := ifNoneMatch(options)
	if err != nil {
		return nil, "", err
	}

	params, err := optionsToParams(options)
	if err != nil {
		return nil, "", err
	}
	opts := &chttp.Options{
		Accept:      "application/json",
		IfNoneMatch: inm,
	}
	resp, err := d.Client.DoReq(ctx, method, d.path(chttp.EncodeDocID(docID), params), opts)
	if err != nil {
		return nil, "", err
	}
	if respErr := chttp.ResponseError(resp); respErr != nil {
		return nil, "", respErr
	}
	rev, err := chttp.GetRev(resp)
	return resp, rev, err
}

func (d *db) CreateDoc(ctx context.Context, doc interface{}, options map[string]interface{}) (docID, rev string, err error) {
	result := struct {
		ID  string `json:"id"`
		Rev string `json:"rev"`
	}{}

	fullCommit, err := fullCommit(false, options)
	if err != nil {
		return "", "", err
	}

	path := d.dbName
	if len(options) > 0 {
		params, e := optionsToParams(options)
		if e != nil {
			return "", "", e
		}
		path += "?" + params.Encode()
	}

	opts := &chttp.Options{
		Body:       chttp.EncodeBody(doc),
		FullCommit: fullCommit,
	}
	_, err = d.Client.DoJSON(ctx, kivik.MethodPost, path, opts, &result)
	return result.ID, result.Rev, err
}

func (d *db) Put(ctx context.Context, docID string, doc interface{}, options map[string]interface{}) (rev string, err error) {
	if docID == "" {
		return "", missingArg("docID")
	}
	fullCommit, err := fullCommit(false, options)
	if err != nil {
		return "", err
	}
	opts := &chttp.Options{
		Body:       chttp.EncodeBody(doc),
		FullCommit: fullCommit,
	}
	var result struct {
		ID  string `json:"id"`
		Rev string `json:"rev"`
	}
	_, err = d.Client.DoJSON(ctx, kivik.MethodPut, d.path(chttp.EncodeDocID(docID), nil), opts, &result)
	if err != nil {
		return "", err
	}
	if result.ID != docID {
		// This should never happen; this is mostly for debugging and internal use
		return result.Rev, errors.Statusf(kivik.StatusBadResponse, "modified document ID (%s) does not match that requested (%s)", result.ID, docID)
	}
	return result.Rev, nil
}

const attachmentsKey = "_attachments"

func extractAttachments(doc interface{}) (*kivik.Attachments, bool) {
	v := reflect.ValueOf(doc)
	if v.Type().Kind() == reflect.Ptr {
		return extractAttachments(v.Elem().Interface())
	}
	if stdMap, ok := doc.(map[string]interface{}); ok {
		return interfaceToAttachments(stdMap[attachmentsKey])
	}
	if v.Kind() != reflect.Struct {
		return nil, false
	}
	for i := 0; i < v.NumField(); i++ {
		if v.Type().Field(i).Tag.Get("json") == attachmentsKey {
			return interfaceToAttachments(v.Field(i).Interface())
		}
	}
	return nil, false
}

func interfaceToAttachments(i interface{}) (*kivik.Attachments, bool) {
	switch t := i.(type) {
	case kivik.Attachments:
		atts := make(kivik.Attachments, len(t))
		for k, v := range t {
			atts[k] = v
			delete(t, k)
		}
		return &atts, true
	case *kivik.Attachments:
		atts := new(kivik.Attachments)
		*atts = *t
		*t = nil
		return atts, true
	}
	return nil, false
}

// multipartAttachments reads a json stream on in, and produces a
// multipart/related output suitable for a PUT request.
func multipartAttachments(in io.ReadCloser, att *kivik.Attachments) (string, io.ReadCloser) {
	r, w := io.Pipe()
	body := multipart.NewWriter(w)
	go func() {
		err := createMultipart(body, in, att)
		e := in.Close()
		if err == nil {
			err = e
		}
		_ = w.CloseWithError(err)
	}()
	return body.Boundary(), r
}

func createMultipart(w *multipart.Writer, r io.ReadCloser, atts *kivik.Attachments) error {
	doc, err := w.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"application/json"},
	})
	if err != nil {
		return err
	}
	attJSON := replaceAttachments(r, atts)
	if _, e := io.Copy(doc, attJSON); e != nil {
		return e
	}

	for filename, att := range *atts {
		file, err := w.CreatePart(textproto.MIMEHeader{
			"Content-Type":        {att.ContentType},
			"Content-Disposition": {fmt.Sprintf(`attachment; filename=%q`, filename)},
		})
		if err != nil {
			return err
		}
		if _, err := io.Copy(file, att); err != nil {
			return err
		}
		_ = att.Close()
	}

	return w.Close()
}

// replaceAttachments reads a json stream on in, looking for the _attachments
// key, then replaces its value with the marshaled version of att.
func replaceAttachments(in io.ReadCloser, atts *kivik.Attachments) io.ReadCloser {
	r, w := io.Pipe()
	go func() {
		err := copyWithAttachments(w, in, attachmentStubs(atts))
		e := in.Close()
		if err == nil {
			err = e
		}
		_ = w.CloseWithError(err)
	}()
	return r
}

func attachmentStubs(atts *kivik.Attachments) map[string]interface{} {
	if atts == nil {
		return nil
	}
	result := make(map[string]interface{}, len(*atts))
	for filename, att := range *atts {
		result[filename] = map[string]interface{}{
			"content_type": att.ContentType,
			"follows":      true,
		}
	}
	return result
}

// setSize sets the attachment's size, if it's not already set
func setSize(att *kivik.Attachment) error {
	if att.Size > 0 {
		return nil
	}
	buf := new(bytes.Buffer)
	n, err := buf.ReadFrom(att.Content)
	att.Size = n
	att.Content = ioutil.NopCloser(buf)
	return err
}

func copyWithAttachments(w io.Writer, r io.Reader, att map[string]interface{}) error {
	dec := json.NewDecoder(r)
	t, err := dec.Token()
	if err == nil {
		if t != json.Delim('{') {
			return fmt.Errorf("expected '{', found '%v'", t)
		}
	}
	if err != nil {
		if err != io.EOF {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "%v", t); err != nil {
		return err
	}
	first := true
	for {
		t, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch tp := t.(type) {
		case string:
			if !first {
				if _, e := w.Write([]byte(",")); e != nil {
					return e
				}
			}
			first = false
			if _, e := fmt.Fprintf(w, `"%s":`, tp); e != nil {
				return e
			}
			var val json.RawMessage
			if e := dec.Decode(&val); e != nil {
				return e
			}
			if tp == attachmentsKey {
				if e := json.NewEncoder(w).Encode(att); e != nil {
					return e
				}
				// Once we're here, we can just stream the rest of the input
				// unaltered.
				if _, e := io.Copy(w, dec.Buffered()); e != nil {
					return e
				}
				_, e := io.Copy(w, r)
				return e
			}
			if _, e := w.Write(val); e != nil {
				return e
			}
		case json.Delim:
			if tp != json.Delim('}') {
				return fmt.Errorf("expected '}', found '%v'", t)
			}
			if _, err := fmt.Fprintf(w, "%v", t); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *db) Delete(ctx context.Context, docID, rev string, options map[string]interface{}) (string, error) {
	if docID == "" {
		return "", missingArg("docID")
	}
	if rev == "" {
		return "", missingArg("rev")
	}

	fullCommit, err := fullCommit(false, options)
	if err != nil {
		return "", err
	}

	query, err := optionsToParams(options)
	if err != nil {
		return "", err
	}
	query.Add("rev", rev)
	opts := &chttp.Options{
		FullCommit: fullCommit,
	}
	resp, err := d.Client.DoReq(ctx, kivik.MethodDelete, d.path(chttp.EncodeDocID(docID), query), opts)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	return chttp.GetRev(resp)
}

func (d *db) Flush(ctx context.Context) error {
	_, err := d.Client.DoError(ctx, kivik.MethodPost, d.path("/_ensure_full_commit", nil), nil)
	return err
}

func (d *db) Stats(ctx context.Context) (*driver.DBStats, error) {
	result := struct {
		driver.DBStats
		Sizes struct {
			File     int64 `json:"file"`
			External int64 `json:"external"`
			Active   int64 `json:"active"`
		} `json:"sizes"`
		UpdateSeq json.RawMessage `json:"update_seq"`
	}{}
	_, err := d.Client.DoJSON(ctx, kivik.MethodGet, d.dbName, nil, &result)
	stats := result.DBStats
	if result.Sizes.File > 0 {
		stats.DiskSize = result.Sizes.File
	}
	if result.Sizes.External > 0 {
		stats.ExternalSize = result.Sizes.External
	}
	if result.Sizes.Active > 0 {
		stats.ActiveSize = result.Sizes.Active
	}
	stats.UpdateSeq = string(bytes.Trim(result.UpdateSeq, `"`))
	return &stats, err
}

func (d *db) Compact(ctx context.Context) error {
	res, err := d.Client.DoReq(ctx, kivik.MethodPost, d.path("/_compact", nil), nil)
	if err != nil {
		return err
	}
	return chttp.ResponseError(res)
}

func (d *db) CompactView(ctx context.Context, ddocID string) error {
	if ddocID == "" {
		return missingArg("ddocID")
	}
	res, err := d.Client.DoReq(ctx, kivik.MethodPost, d.path("/_compact/"+ddocID, nil), nil)
	if err != nil {
		return err
	}
	return chttp.ResponseError(res)
}

func (d *db) ViewCleanup(ctx context.Context) error {
	res, err := d.Client.DoReq(ctx, kivik.MethodPost, d.path("/_view_cleanup", nil), nil)
	if err != nil {
		return err
	}
	return chttp.ResponseError(res)
}

func (d *db) Security(ctx context.Context) (*driver.Security, error) {
	var sec *driver.Security
	_, err := d.Client.DoJSON(ctx, kivik.MethodGet, d.path("/_security", nil), nil, &sec)
	return sec, err
}

func (d *db) SetSecurity(ctx context.Context, security *driver.Security) error {
	opts := &chttp.Options{
		Body: chttp.EncodeBody(security),
	}
	res, err := d.Client.DoReq(ctx, kivik.MethodPut, d.path("/_security", nil), opts)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	return chttp.ResponseError(res)
}

func (d *db) Copy(ctx context.Context, targetID, sourceID string, options map[string]interface{}) (targetRev string, err error) {
	if sourceID == "" {
		return "", errors.Status(kivik.StatusBadRequest, "kivik: sourceID required")
	}
	if targetID == "" {
		return "", errors.Status(kivik.StatusBadRequest, "kivik: targetID required")
	}
	fullCommit, err := fullCommit(false, options)
	if err != nil {
		return "", err
	}
	params, err := optionsToParams(options)
	if err != nil {
		return "", err
	}
	opts := &chttp.Options{
		FullCommit:  fullCommit,
		Destination: targetID,
	}
	resp, err := d.Client.DoReq(ctx, kivik.MethodCopy, d.path(chttp.EncodeDocID(sourceID), params), opts)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() // nolint: errcheck
	return chttp.GetRev(resp)
}
