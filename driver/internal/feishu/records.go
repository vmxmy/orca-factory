package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/vmxmy/orca-factory/driver/internal/infra"
)

type Field struct {
	ID       string
	Name     string
	Type     string
	Multiple bool
	Options  []string
}

// Record holds raw cell values keyed by field ID; field names may be renamed.
type Record struct {
	ID     string
	Fields map[string]json.RawMessage
}

// Page is a column-oriented response turned into records. Rev is the table
// revision at read time.
type Page struct {
	Records    []Record
	FieldNames map[string]string
	FieldTypes map[string]string
	HasMore    bool
	Rev        int64
}

type IgnoredField struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// WriteResult carries no revision: base/v3 write responses do not return one.
type WriteResult struct {
	RecordIDs     []string
	IgnoredFields []IgnoredField
}

func (c *Client) Fields(ctx context.Context, base, table string) ([]Field, error) {
	var data struct {
		Fields []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Type     string `json:"type"`
			Multiple bool   `json:"multiple"`
			Options  []struct {
				Name string `json:"name"`
			} `json:"options"`
		} `json:"fields"`
	}
	if err := c.call(ctx, http.MethodGet, tablePath(base, table)+"/fields", nil, &data); err != nil {
		return nil, err
	}
	fields := make([]Field, 0, len(data.Fields))
	for _, f := range data.Fields {
		options := make([]string, 0, len(f.Options))
		for _, o := range f.Options {
			options = append(options, o.Name)
		}
		fields = append(fields, Field{ID: f.ID, Name: f.Name, Type: f.Type, Multiple: f.Multiple, Options: options})
	}
	return fields, nil
}

type Table struct {
	ID   string
	Name string
}

// Condition is one server-side filter clause, sent as [field, operator, value].
// Field is the field name; link values are []LinkRef.
type Condition struct {
	Field    string
	Operator string
	Value    any
}

func (c Condition) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{c.Field, c.Operator, c.Value})
}

type Filter struct {
	Logic      string      `json:"logic"`
	Conditions []Condition `json:"conditions"`
}

// LinkRef is a link cell entry and a link filter value.
type LinkRef struct {
	ID string `json:"id"`
}

func (c *Client) Tables(ctx context.Context, base string) ([]Table, error) {
	var data struct {
		Tables []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"tables"`
	}
	if err := c.call(ctx, http.MethodGet, "/open-apis/base/v3/bases/"+url.PathEscape(base)+"/tables", nil, &data); err != nil {
		return nil, err
	}
	tables := make([]Table, 0, len(data.Tables))
	for _, t := range data.Tables {
		tables = append(tables, Table{ID: t.ID, Name: t.Name})
	}
	return tables, nil
}

func (c *Client) ListRecords(ctx context.Context, base, table string, offset, limit int) (Page, error) {
	query := url.Values{"offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(limit)}}
	return c.readPage(ctx, http.MethodGet, recordsPath(base, table)+"?"+query.Encode(), nil)
}

// ListRecordsWhere lists only records matching filter.
func (c *Client) ListRecordsWhere(ctx context.Context, base, table string, filter Filter, offset, limit int) (Page, error) {
	raw, err := json.Marshal(filter)
	if err != nil {
		return Page{}, safeError(err)
	}
	query := url.Values{"filter": {string(raw)}, "offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(limit)}}
	return c.readPage(ctx, http.MethodGet, recordsPath(base, table)+"?"+query.Encode(), nil)
}

func (c *Client) BatchGet(ctx context.Context, base, table string, recordIDs []string) (Page, error) {
	return c.readPage(ctx, http.MethodPost, recordsPath(base, table)+"/batch_get", map[string]any{"record_id_list": recordIDs})
}

// BatchCreate writes cells keyed by field name or ID. Read-only fields are
// skipped by the service and reported in IgnoredFields.
func (c *Client) BatchCreate(ctx context.Context, base, table string, records []map[string]any) (WriteResult, error) {
	return c.writeRecords(ctx, base, table, "batch_create", map[string]any{"create_records": records})
}

// BatchUpdate writes cells per record ID.
func (c *Client) BatchUpdate(ctx context.Context, base, table string, records map[string]map[string]any) (WriteResult, error) {
	return c.writeRecords(ctx, base, table, "batch_update", map[string]any{"update_records": records})
}

// DeleteResult reports IDs the service could not yet find as well as requested IDs.
type DeleteResult struct {
	RecordIDs []string `json:"record_id_list"`
	NotFound  []string `json:"record_not_found"`
}

func (c *Client) BatchDelete(ctx context.Context, base, table string, recordIDs []string) (DeleteResult, error) {
	var data DeleteResult
	if err := c.write(ctx, base, table, "batch_delete", map[string]any{"record_id_list": recordIDs}, &data); err != nil {
		return DeleteResult{}, err
	}
	return data, nil
}

func (c *Client) writeRecords(ctx context.Context, base, table, action string, input any) (WriteResult, error) {
	var data struct {
		RecordIDs     []string       `json:"record_id_list"`
		IgnoredFields []IgnoredField `json:"ignored_fields"`
	}
	if err := c.write(ctx, base, table, action, input, &data); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{RecordIDs: data.RecordIDs, IgnoredFields: data.IgnoredFields}, nil
}

func (c *Client) readPage(ctx context.Context, method, path string, input any) (Page, error) {
	var data struct {
		Rows       [][]json.RawMessage `json:"data"`
		FieldIDs   []string            `json:"field_id_list"`
		FieldTypes []string            `json:"field_type_list"`
		FieldNames []string            `json:"fields"`
		RecordIDs  []string            `json:"record_id_list"`
		HasMore    bool                `json:"has_more"`
		Rev        int64               `json:"rev"`
	}
	if err := c.call(ctx, method, path, input, &data); err != nil {
		return Page{}, err
	}
	malformed := len(data.FieldNames) != len(data.FieldIDs) || len(data.FieldTypes) != len(data.FieldIDs) || len(data.Rows) != len(data.RecordIDs)
	for _, row := range data.Rows {
		malformed = malformed || len(row) != len(data.FieldIDs)
	}
	if malformed {
		return Page{}, infra.Classify(errors.New("feishu records response malformed"))
	}
	page := Page{
		Records:    make([]Record, 0, len(data.RecordIDs)),
		FieldNames: make(map[string]string, len(data.FieldIDs)),
		FieldTypes: make(map[string]string, len(data.FieldIDs)),
		HasMore:    data.HasMore,
		Rev:        data.Rev,
	}
	for i, id := range data.FieldIDs {
		page.FieldNames[id] = data.FieldNames[i]
		page.FieldTypes[id] = data.FieldTypes[i]
	}
	for i, id := range data.RecordIDs {
		cells := make(map[string]json.RawMessage, len(data.FieldIDs))
		for j, field := range data.FieldIDs {
			cells[field] = data.Rows[i][j]
		}
		page.Records = append(page.Records, Record{ID: id, Fields: cells})
	}
	return page, nil
}
