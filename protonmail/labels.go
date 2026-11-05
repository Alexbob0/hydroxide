package protonmail

import (
	"net/http"
	"net/url"
	"strconv"
)

const (
	LabelInbox    = "0"
	LabelAllDraft = "1"
	LabelAllSent  = "2"
	LabelTrash    = "3"
	LabelSpam     = "4"
	LabelAllMail  = "5"
	LabelArchive  = "6"
	LabelSent     = "7"
	LabelDraft    = "8"
	LabelStarred  = "10"
)

type LabelType int

const (
	LabelMessage LabelType = 1
	LabelContact LabelType = 2
	LabelFolder  LabelType = 3
	LabelSystem  LabelType = 4
)

type Label struct {
	ID        string
	ParentID  string
	Name      string
	Path      string // e.g. "Parent/Child", only populated for folders
	Color     string
	Display   int
	Type      LabelType
	Exclusive int
	Notify    int
	Order     int
}

// IsFolder returns true if the label is a folder: a message can be in at most
// one folder at a time.
//
// Older API versions returned folders as message labels with Exclusive set to
// 1, current versions return them with Type set to LabelFolder.
func (label *Label) IsFolder() bool {
	return label.Type == LabelFolder || label.Exclusive == 1
}

// ListLabels lists labels. If types are specified, only labels of these types
// are returned.
func (c *Client) ListLabels(types ...LabelType) ([]*Label, error) {
	if len(types) == 0 {
		return c.listLabels(nil)
	}

	var labels []*Label
	seen := make(map[string]struct{})
	for _, t := range types {
		v := url.Values{}
		v.Set("Type", strconv.Itoa(int(t)))
		l, err := c.listLabels(v)
		if err != nil {
			return nil, err
		}

		for _, label := range l {
			if _, ok := seen[label.ID]; ok {
				continue
			}
			seen[label.ID] = struct{}{}
			labels = append(labels, label)
		}
	}
	return labels, nil
}

func (c *Client) listLabels(v url.Values) ([]*Label, error) {
	path := "/labels"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}

	req, err := c.newRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var respData struct {
		resp
		Labels []*Label
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}

	return respData.Labels, nil
}

type CreateLabelReq struct {
	Name     string
	Color    string
	Type     LabelType
	ParentID string `json:",omitempty"`
}

func (c *Client) CreateLabel(label *CreateLabelReq) (*Label, error) {
	req, err := c.newJSONRequest(http.MethodPost, "/labels", label)
	if err != nil {
		return nil, err
	}

	var respData struct {
		resp
		Label *Label
	}
	if err := c.doJSON(req, &respData); err != nil {
		return nil, err
	}

	return respData.Label, nil
}
