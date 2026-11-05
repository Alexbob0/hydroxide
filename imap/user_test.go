package imap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"

	"github.com/emersion/hydroxide/imap/database"
	"github.com/emersion/hydroxide/protonmail"
)

// testAPI is a minimal Proton API serving labels.
type testAPI struct {
	sync.Mutex
	labels  []*protonmail.Label
	created []protonmail.CreateLabelReq
}

func (api *testAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.Lock()
	defer api.Unlock()

	resp := map[string]interface{}{"Code": 1000}
	switch {
	case r.URL.Path == "/labels" && r.Method == http.MethodGet:
		labels := []*protonmail.Label{}
		for _, label := range api.labels {
			if t := r.URL.Query().Get("Type"); t == "" || t == strconv.Itoa(int(label.Type)) {
				labels = append(labels, label)
			}
		}
		resp["Labels"] = labels
	case r.URL.Path == "/labels" && r.Method == http.MethodPost:
		var req protonmail.CreateLabelReq
		json.NewDecoder(r.Body).Decode(&req)
		api.created = append(api.created, req)
		label := &protonmail.Label{
			ID:       fmt.Sprintf("new%v", len(api.created)),
			Name:     req.Name,
			Path:     req.Name,
			ParentID: req.ParentID,
			Type:     req.Type,
		}
		for _, parent := range api.labels {
			if parent.ID == req.ParentID {
				label.Path = parent.Path + "/" + req.Name
			}
		}
		api.labels = append(api.labels, label)
		resp["Label"] = label
	case r.URL.Path == "/messages/count":
		resp["Counts"] = []*protonmail.MessageCount{{LabelID: "c", Total: 2, Unread: 1}}
	default:
		w.WriteHeader(http.StatusNotFound)
		resp["Code"] = 404
		resp["Error"] = "not found"
	}
	json.NewEncoder(w).Encode(resp)
}

func (api *testAPI) setLabels(labels ...*protonmail.Label) {
	api.Lock()
	api.labels = labels
	api.Unlock()
}

func newTestUser(t *testing.T, labels ...*protonmail.Label) (*user, *testAPI) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	api := &testAPI{labels: labels}
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	db, err := database.Open("test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	u := &user{c: &protonmail.Client{RootURL: ts.URL}, db: db}
	if err := u.initMailboxes(); err != nil {
		t.Fatalf("initMailboxes() = %v", err)
	}
	return u, api
}

func folder(id, name, path, parentID string) *protonmail.Label {
	return &protonmail.Label{ID: id, Name: name, Path: path, ParentID: parentID, Type: protonmail.LabelFolder}
}

var testLabels = []*protonmail.Label{
	folder("p", "Parent", "Parent", ""),
	folder("c", "Child", "Parent/Child", "p"),
	folder("a", "Impôts", "Impôts", ""),
	{ID: "o", Name: "Old folder", Type: protonmail.LabelMessage, Exclusive: 1},
	{ID: "l", Name: "Follow up", Type: protonmail.LabelMessage},
}

// userMailboxes returns the names and attributes of the non-system mailboxes.
func userMailboxes(t *testing.T, u *user) map[string][]string {
	list, err := u.ListMailboxes(false)
	if err != nil {
		t.Fatal(err)
	}
	mailboxes := make(map[string][]string)
	for _, mbox := range list {
		info, err := mbox.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Delimiter != "/" {
			t.Errorf("mailbox %q: delimiter = %q", info.Name, info.Delimiter)
		}
		if !isSystemLabel(mbox.(*mailbox).label) {
			mailboxes[info.Name] = info.Attributes
		}
	}
	return mailboxes
}

func TestFolders(t *testing.T) {
	u, _ := newTestUser(t, testLabels...)

	want := map[string][]string{
		"Parent":       {imap.HasChildrenAttr},
		"Parent/Child": {imap.HasNoChildrenAttr},
		"Impôts":       {imap.HasNoChildrenAttr},
		"Old folder":   {imap.HasNoChildrenAttr},
	}
	if got := userMailboxes(t, u); !reflect.DeepEqual(got, want) {
		t.Errorf("mailboxes = %v, want %v", got, want)
	}

	info, _ := u.getMailbox(imap.InboxName).Info()
	if !reflect.DeepEqual(info.Attributes, []string{imap.NoInferiorsAttr}) {
		t.Errorf("INBOX attributes = %v", info.Attributes)
	}

	if got := u.getMailbox("Parent/Child"); got == nil || got.label != "c" || got.total != 2 {
		t.Errorf("mailbox Parent/Child = %+v, want label c with 2 messages", got)
	}
	if got := u.flags["l"]; got != "Follow_up" {
		t.Errorf("flag of label l = %q, want Follow_up", got)
	}
}

func TestFolderChanges(t *testing.T) {
	u, api := newTestUser(t, testLabels...)

	// Created in the web client: found when looked up, without any event
	api.setLabels(append(testLabels, folder("n", "New", "New", ""))...)
	if mbox, err := u.GetMailbox("New"); err != nil || mbox.(*mailbox).label != "n" {
		t.Fatalf("GetMailbox(New) = %v, %v", mbox, err)
	}
	if _, err := u.GetMailbox("Nowhere"); err != imapbackend.ErrNoSuchMailbox {
		t.Errorf("GetMailbox(Nowhere) = %v, want ErrNoSuchMailbox", err)
	}

	// Parent renamed, New deleted, Follow up made a folder
	api.setLabels(
		folder("p", "Parents", "Parents", ""),
		folder("c", "Child", "Parents/Child", "p"),
		folder("a", "Impôts", "Impôts", ""),
		folder("l", "Follow up", "Follow up", ""),
	)
	if err := u.refreshLabels(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for name := range userMailboxes(t, u) {
		got = append(got, name)
	}
	sort.Strings(got)
	if want := []string{"Follow up", "Impôts", "Parents", "Parents/Child"}; !reflect.DeepEqual(got, want) {
		t.Errorf("mailboxes = %v, want %v", got, want)
	}
	if _, ok := u.flags["l"]; ok {
		t.Errorf("label l is still a flag")
	}
	if u.flags[protonmail.LabelStarred] != imap.FlaggedFlag {
		t.Errorf("system flags = %v", u.flags)
	}
}

func TestCreateMailbox(t *testing.T) {
	u, api := newTestUser(t, testLabels...)

	for _, name := range []string{"Top", "Parent/Sub", "A/B/C/"} {
		if err := u.CreateMailbox(name); err != nil {
			t.Errorf("CreateMailbox(%q) = %v", name, err)
		}
	}

	want := []protonmail.CreateLabelReq{
		{Name: "Top", Color: folderColor, Type: protonmail.LabelFolder},
		{Name: "Sub", Color: folderColor, Type: protonmail.LabelFolder, ParentID: "p"},
		{Name: "A", Color: folderColor, Type: protonmail.LabelFolder},
		{Name: "B", Color: folderColor, Type: protonmail.LabelFolder, ParentID: "new3"},
		{Name: "C", Color: folderColor, Type: protonmail.LabelFolder, ParentID: "new4"},
	}
	if !reflect.DeepEqual(api.created, want) {
		t.Errorf("created labels = %+v, want %+v", api.created, want)
	}
	for _, name := range []string{"Top", "Parent/Sub", "A", "A/B", "A/B/C"} {
		if u.getMailbox(name) == nil {
			t.Errorf("mailbox %q not found after CREATE", name)
		}
	}

	for _, name := range []string{"Parent/Child", "INBOX", "INBOX/Sub", "Archive/Sub", "", "/"} {
		if err := u.CreateMailbox(name); err == nil {
			t.Errorf("CreateMailbox(%q) = nil, want an error", name)
		}
	}
	if len(api.created) != len(want) {
		t.Errorf("created labels = %+v, want no more", api.created[len(want):])
	}
}

func TestGetFlag(t *testing.T) {
	u, _ := newTestUser(t, testLabels...)

	// go-imap lower-cases keywords
	for _, flag := range []string{"Follow_up", "follow_up", imap.CanonicalFlag("Follow_up")} {
		if got := u.getFlag(flag); got != "l" {
			t.Errorf("getFlag(%q) = %q, want l", flag, got)
		}
	}
	if got := u.getFlag("unknown"); got != "" {
		t.Errorf("getFlag(unknown) = %q", got)
	}
}
