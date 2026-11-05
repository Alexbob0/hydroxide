package imap

import (
	"errors"
	"log"
	"strings"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/emersion/go-imap"
	imapbackend "github.com/emersion/go-imap/backend"
	"github.com/emersion/hydroxide/events"
	"github.com/emersion/hydroxide/imap/database"
	"github.com/emersion/hydroxide/protonmail"
)

var systemMailboxes = []struct {
	name  string
	label string
	attrs []string
}{
	{imap.InboxName, protonmail.LabelInbox, nil},
	{"All Mail", protonmail.LabelAllMail, []string{imap.AllAttr}},
	{"Archive", protonmail.LabelArchive, []string{imap.ArchiveAttr}},
	{"Drafts", protonmail.LabelDraft, []string{imap.DraftsAttr}},
	{"Starred", protonmail.LabelStarred, []string{imap.FlaggedAttr}},
	{"Spam", protonmail.LabelSpam, []string{imap.JunkAttr}},
	{"Sent", protonmail.LabelSent, []string{imap.SentAttr}},
	{"Trash", protonmail.LabelTrash, []string{imap.TrashAttr}},
}

var systemFlags = []struct {
	name  string
	label string
}{
	{imap.FlaggedFlag, protonmail.LabelStarred},
	{imap.DraftFlag, protonmail.LabelDraft},
}

type user struct {
	username    string
	backend     *backend
	c           *protonmail.Client
	u           *protonmail.User
	privateKeys openpgp.EntityList
	addrs       []*protonmail.Address

	db             *database.User
	eventsReceiver *events.Receiver

	done      chan<- struct{}
	eventSent chan struct{}

	sync.Mutex // protects everything below

	numClients int
	mailboxes  map[string]*mailbox // indexed by label ID
	flags      map[string]string   // indexed by label ID
}

func getUser(be *backend, username string, c *protonmail.Client, privateKeys openpgp.EntityList) (*user, error) {
	// TODO: logging a user in may take some time, find a way not to lock all
	// other logins during this time
	be.Lock()
	defer be.Unlock()

	if u, ok := be.users[username]; ok {
		u.Lock()
		u.numClients++
		u.Unlock()
		return u, nil
	} else {
		u, err := newUser(be, username, c, privateKeys)
		if err != nil {
			return nil, err
		}

		be.users[username] = u
		return u, nil
	}
}

func newUser(be *backend, username string, c *protonmail.Client, privateKeys openpgp.EntityList) (*user, error) {
	u, err := c.GetCurrentUser()
	if err != nil {
		return nil, err
	}

	addrs, err := c.ListAddresses()
	if err != nil {
		return nil, err
	}

	uu := &user{
		username:    username,
		backend:     be,
		c:           c,
		u:           u,
		privateKeys: privateKeys,
		addrs:       addrs,
		eventSent:   make(chan struct{}),
		numClients:  1,
	}

	db, err := database.Open(u.Name + ".db")
	if err != nil {
		return nil, err
	}
	uu.db = db

	if err := uu.initMailboxes(); err != nil {
		return nil, err
	}

	done := make(chan struct{})
	uu.done = done
	ch := make(chan *protonmail.Event)
	go uu.receiveEvents(be.updates, ch)
	uu.eventsReceiver = be.eventsManager.Register(c, u.Name, ch, done)

	log.Printf("User %q logged in via IMAP", u.Name)
	return uu, nil
}

func labelNameToFlag(s string) string {
	var sb strings.Builder
	var lastValid bool
	for _, r := range s {
		// See atom-specials in RFC 3501
		var valid bool
		switch r {
		case '(', ')', '{':
		case ' ', '\t': // SP
		case '%', '*': // list-wildcards
		case '"', '\\': // quoted-specials
		case ']': // resp-specials
		default:
			valid = r <= '~' && r > 31
		}
		if !valid {
			if !lastValid {
				continue
			}
			r = '_'
		}
		sb.WriteRune(r)
		lastValid = valid
	}
	return sb.String()
}

// folderName returns the mailbox name of a folder. Sub-folders are separated
// from their parent folder by the hierarchy delimiter, e.g. "Parent/Child".
func folderName(label *protonmail.Label) string {
	if label.Path != "" {
		return label.Path
	}
	return label.Name
}

func isSystemLabel(labelID string) bool {
	for _, data := range systemMailboxes {
		if data.label == labelID {
			return true
		}
	}
	return false
}

func (u *user) initMailboxes() error {
	u.Lock()
	defer u.Unlock()

	u.mailboxes = make(map[string]*mailbox)
	for _, data := range systemMailboxes {
		var err error
		u.mailboxes[data.label], err = newMailbox(data.name, data.label, data.attrs, u)
		if err != nil {
			return err
		}
	}

	u.flags = make(map[string]string)
	for _, data := range systemFlags {
		u.flags[data.label] = data.name
	}

	labels, err := u.c.ListLabels(protonmail.LabelMessage, protonmail.LabelFolder)
	if err != nil {
		return err
	}

	if _, err := u.updateLabels(labels); err != nil {
		return err
	}

	counts, err := u.c.CountMessages("")
	if err != nil {
		return err
	}

	for _, count := range counts {
		if mbox, ok := u.mailboxes[count.LabelID]; ok {
			mbox.total = count.Total
			mbox.unread = count.Unread
		}
	}

	return nil
}

// updateLabels updates mailboxes and flags to match the user's labels: folders
// become mailboxes, other labels become flags. It returns the mailboxes that
// have been added. The caller must hold the user lock.
func (u *user) updateLabels(labels []*protonmail.Label) ([]*mailbox, error) {
	var added []*mailbox
	seen := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		if isSystemLabel(label.ID) {
			continue
		}
		seen[label.ID] = struct{}{}

		if label.IsFolder() {
			delete(u.flags, label.ID)

			name := folderName(label)
			if mbox, ok := u.mailboxes[label.ID]; ok && mbox.name == name {
				continue
			}

			// New or renamed folder
			mbox, err := newMailbox(name, label.ID, nil, u)
			if err != nil {
				return added, err
			}
			u.mailboxes[label.ID] = mbox
			added = append(added, mbox)
		} else {
			delete(u.mailboxes, label.ID)
			u.flags[label.ID] = labelNameToFlag(label.Name)
		}
	}

	for labelID := range u.mailboxes {
		if _, ok := seen[labelID]; !ok && !isSystemLabel(labelID) {
			delete(u.mailboxes, labelID)
		}
	}
	for labelID := range u.flags {
		if _, ok := seen[labelID]; !ok && !isSystemLabel(labelID) {
			delete(u.flags, labelID)
		}
	}

	return added, nil
}

// refreshLabels fetches the user's labels and updates mailboxes and flags.
func (u *user) refreshLabels() error {
	labels, err := u.c.ListLabels(protonmail.LabelMessage, protonmail.LabelFolder)
	if err != nil {
		return err
	}

	u.Lock()
	added, err := u.updateLabels(labels)
	u.Unlock()
	if err != nil || len(added) == 0 {
		return err
	}

	counts, err := u.c.CountMessages("")
	if err != nil {
		return err
	}

	for _, mbox := range added {
		for _, count := range counts {
			if count.LabelID == mbox.label {
				mbox.Lock()
				mbox.total = count.Total
				mbox.unread = count.Unread
				mbox.Unlock()
			}
		}
	}
	return nil
}

func (u *user) Username() string {
	return u.u.Name
}

func (u *user) ListMailboxes(subscribed bool) ([]imapbackend.Mailbox, error) {
	u.Lock()
	defer u.Unlock()

	list := make([]imapbackend.Mailbox, 0, len(u.mailboxes))
	for _, mbox := range u.mailboxes {
		list = append(list, mbox)
	}
	return list, nil
}

func (u *user) getMailboxByLabel(labelID string) *mailbox {
	u.Lock()
	defer u.Unlock()
	return u.mailboxes[labelID]
}

func (u *user) getMailbox(name string) *mailbox {
	u.Lock()
	defer u.Unlock()

	for _, mbox := range u.mailboxes {
		if mbox.name == name {
			return mbox
		}
	}
	return nil
}

func (u *user) hasChildren(name string) bool {
	u.Lock()
	defer u.Unlock()

	prefix := name + delimiter
	for _, mbox := range u.mailboxes {
		if strings.HasPrefix(mbox.name, prefix) {
			return true
		}
	}
	return false
}

// lookupMailbox is like getMailbox, but if the mailbox doesn't exist it
// fetches labels again: the folder may have been created since the last event.
func (u *user) lookupMailbox(name string) *mailbox {
	if mbox := u.getMailbox(name); mbox != nil {
		return mbox
	}

	if err := u.refreshLabels(); err != nil {
		log.Printf("cannot refresh labels: %v", err)
		return nil
	}
	return u.getMailbox(name)
}

func (u *user) GetMailbox(name string) (imapbackend.Mailbox, error) {
	mbox := u.lookupMailbox(name)
	if mbox == nil {
		return nil, imapbackend.ErrNoSuchMailbox
	}
	return mbox, nil
}

func (u *user) getFlag(name string) string {
	u.Lock()
	defer u.Unlock()

	for label, flag := range u.flags {
		if flag == name {
			return label
		}
	}
	return ""
}

// folderColor is the color of folders created via IMAP.
const folderColor = "#f66"

func (u *user) CreateMailbox(name string) error {
	name = strings.TrimSuffix(name, delimiter)
	if name == "" {
		return errors.New("invalid mailbox name")
	}
	if u.lookupMailbox(name) != nil {
		return imapbackend.ErrMailboxAlreadyExists
	}

	leaf := name
	var parentID string
	if i := strings.LastIndex(name, delimiter); i >= 0 {
		parentName := name[:i]
		leaf = name[i+1:]

		parent := u.getMailbox(parentName)
		if parent == nil {
			// Create superior hierarchical names, as suggested by RFC 3501
			if err := u.CreateMailbox(parentName); err != nil {
				return err
			}
			if parent = u.getMailbox(parentName); parent == nil {
				return imapbackend.ErrNoSuchMailbox
			}
		}
		if isSystemLabel(parent.label) {
			return errors.New("cannot create a folder inside a system mailbox")
		}
		parentID = parent.label
	}
	if leaf == "" {
		return errors.New("invalid mailbox name")
	}

	label, err := u.c.CreateLabel(&protonmail.CreateLabelReq{
		Name:     leaf,
		Color:    folderColor,
		Type:     protonmail.LabelFolder,
		ParentID: parentID,
	})
	if err != nil {
		return err
	}

	u.Lock()
	defer u.Unlock()

	mbox, err := newMailbox(name, label.ID, nil, u)
	if err != nil {
		return err
	}
	u.mailboxes[label.ID] = mbox
	return nil
}

func (u *user) DeleteMailbox(name string) error {
	return errNotYetImplemented // TODO
}

func (u *user) RenameMailbox(existingName, newName string) error {
	return errNotYetImplemented // TODO
}

func (u *user) Logout() error {
	u.backend.Lock()
	defer u.backend.Unlock()
	u.Lock()
	defer u.Unlock()

	if u.numClients <= 0 {
		panic("unreachable")
	}
	u.numClients--
	if u.numClients > 0 {
		return nil
	}

	delete(u.backend.users, u.username)

	close(u.done)

	if err := u.db.Close(); err != nil {
		return err
	}

	log.Printf("User %q logged out via IMAP", u.u.Name)
	u.c = nil
	u.u = nil
	u.privateKeys = nil
	return nil
}

func (u *user) poll() {
	go u.eventsReceiver.Poll()
	<-u.eventSent
}

func (u *user) receiveEvents(updates chan<- imapbackend.Update, events <-chan *protonmail.Event) {
	for event := range events {
		var eventUpdates []imapbackend.Update

		if event.Refresh&protonmail.EventRefreshMail != 0 {
			log.Println("Reinitializing the whole IMAP database")

			u.Lock()
			for _, mbox := range u.mailboxes {
				if err := mbox.reset(); err != nil {
					log.Printf("cannot reset mailbox %s: %v", mbox.name, err)
				}
			}
			u.Unlock()

			if err := u.db.ResetMessages(); err != nil {
				log.Printf("cannot reset user: %v", err)
			}

			if err := u.initMailboxes(); err != nil {
				log.Printf("cannot reinitialize mailboxes: %v", err)
			}
		} else {
			if len(event.Labels) > 0 {
				// A folder may have been created, renamed or deleted:
				// handle that before messages are added to it
				log.Println("Received label events, refreshing labels")
				if err := u.refreshLabels(); err != nil {
					log.Printf("cannot refresh labels: %v", err)
				}
			}

			for _, eventMessage := range event.Messages {
				switch eventMessage.Action {
				case protonmail.EventCreate:
					log.Println("Received create event for message", eventMessage.ID)
					seqNums, err := u.db.CreateMessage(eventMessage.Created)
					if err != nil {
						log.Printf("cannot handle create event for message %s: cannot create message in local DB: %v", eventMessage.ID, err)
						break
					}

					// TODO: what if the message was already in the local DB?
					for labelID, seqNum := range seqNums {
						if mbox := u.getMailboxByLabel(labelID); mbox != nil {
							update := new(imapbackend.MailboxUpdate)
							update.Update = imapbackend.NewUpdate(u.u.Name, mbox.name)
							update.MailboxStatus = imap.NewMailboxStatus(mbox.name, []imap.StatusItem{imap.StatusMessages})
							update.MailboxStatus.Messages = seqNum
							eventUpdates = append(eventUpdates, update)
						}
					}
				case protonmail.EventUpdate, protonmail.EventUpdateFlags:
					log.Println("Received update event for message", eventMessage.ID)
					createdSeqNums, deletedSeqNums, err := u.db.UpdateMessage(eventMessage.ID, eventMessage.Updated)
					if err != nil {
						log.Printf("cannot handle update event for message %s: cannot update message in local DB: %v", eventMessage.ID, err)
						break
					}

					for labelID, seqNum := range createdSeqNums {
						if mbox := u.getMailboxByLabel(labelID); mbox != nil {
							update := new(imapbackend.MailboxUpdate)
							update.Update = imapbackend.NewUpdate(u.u.Name, mbox.name)
							update.MailboxStatus = imap.NewMailboxStatus(mbox.name, []imap.StatusItem{imap.StatusMessages})
							update.MailboxStatus.Messages = seqNum
							eventUpdates = append(eventUpdates, update)
						}
					}
					for labelID, seqNum := range deletedSeqNums {
						if mbox := u.getMailboxByLabel(labelID); mbox != nil {
							update := new(imapbackend.ExpungeUpdate)
							update.Update = imapbackend.NewUpdate(u.u.Name, mbox.name)
							update.SeqNum = seqNum
							eventUpdates = append(eventUpdates, update)
						}
					}

					// Send message updates
					msg, err := u.db.Message(eventMessage.ID)
					if err != nil {
						log.Printf("cannot handle update event for message %s: cannot get updated message from local DB: %v", eventMessage.ID, err)
						break
					}
					for _, labelID := range msg.LabelIDs {
						if _, created := createdSeqNums[labelID]; created {
							// This message has been added to the label's mailbox
							// No need to send a message update
							continue
						}

						if mbox := u.getMailboxByLabel(labelID); mbox != nil {
							seqNum, _, err := mbox.db.FromApiID(eventMessage.ID)
							if err != nil {
								log.Printf("cannot handle update event for message %s: cannot get message sequence number in %s: %v", eventMessage.ID, mbox.name, err)
								continue
							}

							update := new(imapbackend.MessageUpdate)
							update.Update = imapbackend.NewUpdate(u.u.Name, mbox.name)
							update.Message = imap.NewMessage(seqNum, []imap.FetchItem{imap.FetchFlags})
							update.Message.Flags = mbox.fetchFlags(msg)
							eventUpdates = append(eventUpdates, update)
						}
					}
				case protonmail.EventDelete:
					log.Println("Received delete event for message", eventMessage.ID)
					seqNums, err := u.db.DeleteMessage(eventMessage.ID)
					if err != nil {
						log.Printf("cannot handle delete event for message %s: cannot delete message from local DB: %v", eventMessage.ID, err)
						break
					}

					for labelID, seqNum := range seqNums {
						if mbox := u.getMailboxByLabel(labelID); mbox != nil {
							update := new(imapbackend.ExpungeUpdate)
							update.Update = imapbackend.NewUpdate(u.u.Name, mbox.name)
							update.SeqNum = seqNum
							eventUpdates = append(eventUpdates, update)
						}
					}
				}
			}

			u.Lock()
			for _, count := range event.MessageCounts {
				if mbox, ok := u.mailboxes[count.LabelID]; ok {
					mbox.total = count.Total
					mbox.unread = count.Unread
				}
			}
			u.Unlock()
		}

		for _, update := range eventUpdates {
			updates <- update
		}
		go func() {
			for _, update := range eventUpdates {
				<-update.Done()
			}

			select {
			case u.eventSent <- struct{}{}:
			default:
			}
		}()
	}
}
