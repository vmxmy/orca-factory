package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// SendText sends once as the application. An uncertain response is not retried.
func (c *Client) SendText(ctx context.Context, chat, text string) error {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	return c.call(ctx, http.MethodPost, "/open-apis/im/v1/messages?receive_id_type=chat_id", map[string]string{
		"receive_id": chat, "msg_type": "text", "content": string(content),
	}, nil)
}

// NotificationRecorded checks only the notification marker. It does not accept
// edited execution evidence as ownership or permission to resume work.
func (c *Client) NotificationRecorded(ctx context.Context, base, ticketID, marker string) (bool, error) {
	if strings.Contains(base, "://") {
		u, err := url.Parse(base)
		if err != nil {
			return false, errors.New("invalid notification Base URL")
		}
		_, token, ok := strings.Cut(u.Path, "/base/")
		if !ok || token == "" {
			return false, errors.New("invalid notification Base URL")
		}
		base = strings.TrimRight(token, "/")
	}
	tables, err := c.Tables(ctx, base)
	if err != nil {
		return false, err
	}
	ticketTable, eventTable := "", ""
	for _, table := range tables {
		if table.Name == "票据" {
			ticketTable = table.ID
		}
		if table.Name == "事件" {
			eventTable = table.ID
		}
	}
	if ticketTable == "" || eventTable == "" {
		return false, errors.New("notification tables missing")
	}
	record := ""
	for offset := 0; ; {
		tickets, err := c.ListRecords(ctx, base, ticketTable, offset, 500)
		if err != nil {
			return false, err
		}
		for _, row := range tickets.Records {
			for field, name := range tickets.FieldNames {
				var id string
				if name == "规范ID" && json.Unmarshal(row.Fields[field], &id) == nil && id == ticketID {
					if record != "" {
						return false, errors.New("notification ticket ambiguous")
					}
					record = row.ID
				}
			}
		}
		if !tickets.HasMore {
			break
		}
		if len(tickets.Records) == 0 {
			return false, errors.New("notification ticket page incomplete")
		}
		offset += len(tickets.Records)
	}
	if record == "" {
		return false, errors.New("notification ticket unresolved")
	}
	filter := Filter{Logic: "and", Conditions: []Condition{{Field: "票据", Operator: "intersects", Value: []LinkRef{{ID: record}}}}}
	for offset := 0; ; {
		page, err := c.ListRecordsWhere(ctx, base, eventTable, filter, offset, 500)
		if err != nil {
			return false, err
		}
		for _, row := range page.Records {
			body := ""
			var links []LinkRef
			for field, name := range page.FieldNames {
				if name == "正文" {
					if err := json.Unmarshal(row.Fields[field], &body); err != nil {
						return false, err
					}
				}
				if name == "票据" {
					if err := json.Unmarshal(row.Fields[field], &links); err != nil {
						return false, err
					}
				}
			}
			if body == marker && len(links) == 1 && links[0].ID == record {
				return true, nil
			}
		}
		if !page.HasMore {
			return false, nil
		}
		if len(page.Records) == 0 {
			return false, errors.New("notification event page incomplete")
		}
		offset += len(page.Records)
	}
}
