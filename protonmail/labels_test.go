package protonmail

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestListLabels(t *testing.T) {
	var queries []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		labels := map[string]string{
			"":       `[{"ID":"a","Name":"Old folder","Type":1,"Exclusive":1}]`,
			"Type=1": `[{"ID":"l","Name":"Label","Type":1}]`,
			"Type=3": `[{"ID":"p","Name":"Parent","Path":"Parent","Type":3},` +
				`{"ID":"c","Name":"Child","Path":"Parent/Child","ParentID":"p","Type":3},` +
				`{"ID":"l","Name":"Label","Type":1}]`,
		}[r.URL.RawQuery]
		w.Write([]byte(`{"Code":1000,"Labels":` + labels + `}`))
	}))
	defer ts.Close()
	c := &Client{RootURL: ts.URL}

	labels, err := c.ListLabels()
	if err != nil {
		t.Fatalf("ListLabels() = %v", err)
	}
	if len(labels) != 1 || !labels[0].IsFolder() {
		t.Errorf("ListLabels() = %+v, want one folder", labels)
	}

	queries = nil
	labels, err = c.ListLabels(LabelMessage, LabelFolder)
	if err != nil {
		t.Fatalf("ListLabels(LabelMessage, LabelFolder) = %v", err)
	}
	if want := []string{"Type=1", "Type=3"}; !reflect.DeepEqual(queries, want) {
		t.Errorf("queries = %v, want %v", queries, want)
	}

	var got []string
	for _, label := range labels {
		got = append(got, label.ID+" "+label.Path+" "+label.ParentID)
		if label.IsFolder() != (label.Type == LabelFolder) {
			t.Errorf("label %q: IsFolder() = %v", label.ID, label.IsFolder())
		}
	}
	if want := []string{"l  ", "p Parent ", "c Parent/Child p"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %q, want %q (duplicates removed)", got, want)
	}
}

func TestCreateLabel(t *testing.T) {
	var body map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/labels" {
			t.Errorf("request = %v %v, want POST /labels", r.Method, r.URL.Path)
		}
		body = nil
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"Code":1000,"Label":{"ID":"c","Name":"Child","Path":"Parent/Child","ParentID":"p","Type":3}}`))
	}))
	defer ts.Close()
	c := &Client{RootURL: ts.URL}

	label, err := c.CreateLabel(&CreateLabelReq{Name: "Child", Color: "#f66", Type: LabelFolder, ParentID: "p"})
	if err != nil {
		t.Fatalf("CreateLabel() = %v", err)
	}
	if label.ID != "c" || label.Path != "Parent/Child" {
		t.Errorf("CreateLabel() = %+v", label)
	}
	want := map[string]interface{}{"Name": "Child", "Color": "#f66", "Type": float64(3), "ParentID": "p"}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("request body = %v, want %v", body, want)
	}

	if _, err := c.CreateLabel(&CreateLabelReq{Name: "Top", Color: "#f66", Type: LabelFolder}); err != nil {
		t.Fatalf("CreateLabel() = %v", err)
	}
	if _, ok := body["ParentID"]; ok {
		t.Errorf("request body = %v, want no ParentID for a top-level folder", body)
	}
}

func TestEventLabels(t *testing.T) {
	var event Event
	b := `{"EventID":"e","Labels":[{"ID":"n","Action":1,"Label":{"ID":"n","Name":"New","Path":"New","Type":3}},{"ID":"d","Action":0}]}`
	if err := json.Unmarshal([]byte(b), &event); err != nil {
		t.Fatal(err)
	}
	if len(event.Labels) != 2 || event.Labels[0].Action != EventCreate || event.Labels[0].Label.Path != "New" ||
		event.Labels[1].Action != EventDelete || event.Labels[1].Label != nil {
		t.Errorf("event labels = %+v", event.Labels)
	}
}
