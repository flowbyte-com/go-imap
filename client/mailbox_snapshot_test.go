package client

// Regression tests for the selected MailboxStatus data race (fork issue #3).
//
// A status returned by Select is an immutable snapshot. The reader installs a
// new snapshot for every unilateral EXISTS/RECENT update instead of writing
// into a pointer the application may be reading. Client.Mailbox returns the
// latest snapshot, and MailboxUpdate carries the snapshot that became current.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/go-imap"
)

type selectResult struct {
	mbox *imap.MailboxStatus
	err  error
}

func selectForSnapshotTest(t *testing.T, c *Client, s *serverConn, readOnly bool, untagged, final string) *imap.MailboxStatus {
	t.Helper()

	done := make(chan selectResult, 1)
	go func() {
		mbox, err := c.Select("INBOX", readOnly)
		done <- selectResult{mbox: mbox, err: err}
	}()

	tag, cmd := s.ScanCmd()
	wantCmd := "SELECT INBOX"
	if readOnly {
		wantCmd = "EXAMINE INBOX"
	}
	if cmd != wantCmd {
		t.Fatalf("client sent command %q, want %q", cmd, wantCmd)
	}

	if _, err := s.WriteString(untagged + tag + final); err != nil {
		t.Fatalf("server write: %v", err)
	}

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("Select(%q, %v) = %v", "INBOX", readOnly, result.err)
		}
		if result.mbox == nil {
			t.Fatal("Select returned a nil mailbox status")
		}
		return result.mbox
	case <-time.After(5 * time.Second):
		t.Fatal("Select did not return")
		return nil
	}
}

func nextMailboxUpdate(t *testing.T, updates <-chan Update) *MailboxUpdate {
	t.Helper()

	select {
	case update := <-updates:
		mboxUpdate, ok := update.(*MailboxUpdate)
		if !ok {
			t.Fatalf("update type = %T, want *MailboxUpdate", update)
		}
		return mboxUpdate
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive a mailbox update")
		return nil
	}
}

func TestClient_SelectStatusSnapshotAfterUnilateralUpdates(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	setClientState(c, imap.AuthenticatedState, nil)

	old := selectForSnapshotTest(t, c, s, false, ""+
		"* 172 EXISTS\r\n"+
		"* 1 RECENT\r\n"+
		"* OK [UNSEEN 12] Message 12 is first unseen\r\n"+
		"* OK [UIDVALIDITY 3857529045] UIDs valid\r\n"+
		"* OK [UIDNEXT 4392] Predicted next UID\r\n"+
		"* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)\r\n"+
		"* OK [PERMANENTFLAGS (\\Deleted \\Seen \\*)] Limited\r\n",
		" OK SELECT completed\r\n")

	if old.Messages != 172 || old.Recent != 1 {
		t.Fatalf("initial status = %d messages, %d recent; want 172, 1", old.Messages, old.Recent)
	}
	if old.UidValidity != 3857529045 || old.UidNext != 4392 || old.UnseenSeqNum != 12 {
		t.Fatalf("initial status lost SELECT values: %#v", old)
	}
	if old.ReadOnly {
		t.Fatal("SELECT status is read-only, want read-write")
	}

	updates := make(chan Update, 8)
	c.Updates = updates

	s.WriteString("* 42 EXISTS\r\n")
	existsUpdate := nextMailboxUpdate(t, updates)
	if existsUpdate.Mailbox.Messages != 42 {
		t.Fatalf("EXISTS update Messages = %d, want 42", existsUpdate.Mailbox.Messages)
	}
	if existsUpdate.Mailbox == old {
		t.Fatal("EXISTS update reused the pointer returned by Select")
	}
	if old.Messages != 172 {
		t.Fatalf("status returned by Select changed in place: Messages = %d, want 172", old.Messages)
	}
	if latest := c.Mailbox(); latest != existsUpdate.Mailbox || latest.Messages != 42 {
		t.Fatalf("Client.Mailbox() = %#v, want the latest 42-message snapshot", latest)
	}

	s.WriteString("* 7 RECENT\r\n")
	recentUpdate := nextMailboxUpdate(t, updates)
	if recentUpdate.Mailbox.Recent != 7 {
		t.Fatalf("RECENT update Recent = %d, want 7", recentUpdate.Mailbox.Recent)
	}
	if recentUpdate.Mailbox.Messages != 42 {
		t.Fatalf("RECENT update lost Messages: got %d, want 42", recentUpdate.Mailbox.Messages)
	}
	if recentUpdate.Mailbox == existsUpdate.Mailbox {
		t.Fatal("RECENT update reused the previous update's snapshot")
	}
	if existsUpdate.Mailbox.Recent != 1 {
		t.Fatalf("earlier update snapshot changed in place: Recent = %d, want 1", existsUpdate.Mailbox.Recent)
	}
	if old.Recent != 1 {
		t.Fatalf("status returned by Select changed in place: Recent = %d, want 1", old.Recent)
	}
	if latest := c.Mailbox(); latest != recentUpdate.Mailbox || latest.Recent != 7 {
		t.Fatalf("Client.Mailbox() = %#v, want the latest 7-recent snapshot", latest)
	}
}

func TestClient_ExamineStatusSnapshot(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	setClientState(c, imap.AuthenticatedState, nil)

	old := selectForSnapshotTest(t, c, s, true, ""+
		"* 9 EXISTS\r\n"+
		"* 2 RECENT\r\n"+
		"* OK [UIDVALIDITY 77] UIDs valid\r\n",
		" OK [READ-ONLY] EXAMINE completed\r\n")

	if !old.ReadOnly {
		t.Fatal("EXAMINE status is not read-only")
	}
	if old.Messages != 9 || old.Recent != 2 || old.UidValidity != 77 {
		t.Fatalf("initial EXAMINE status = %#v, want 9 messages, 2 recent, UIDVALIDITY 77", old)
	}

	updates := make(chan Update, 4)
	c.Updates = updates

	s.WriteString("* 11 EXISTS\r\n")
	update := nextMailboxUpdate(t, updates)
	if update.Mailbox.Messages != 11 || !update.Mailbox.ReadOnly {
		t.Fatalf("EXAMINE update = %#v, want 11 messages and read-only", update.Mailbox)
	}
	if old.Messages != 9 || !old.ReadOnly {
		t.Fatalf("EXAMINE status changed in place: %#v", old)
	}
}

func TestClient_MailboxStatusSnapshotItemsAreNotAliased(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	setClientState(c, imap.AuthenticatedState, nil)

	// No initial EXISTS/RECENT, so the selected status does not contain those
	// item keys yet.
	old := selectForSnapshotTest(t, c, s, false,
		"* OK [UIDVALIDITY 99] UIDs valid\r\n",
		" OK SELECT completed\r\n")

	updates := make(chan Update, 4)
	c.Updates = updates

	s.WriteString("* 5 EXISTS\r\n")
	existsUpdate := nextMailboxUpdate(t, updates)

	if _, ok := old.Items[imap.StatusMessages]; ok {
		t.Fatal("EXISTS update added StatusMessages to the status returned by Select")
	}
	if _, ok := existsUpdate.Mailbox.Items[imap.StatusMessages]; !ok {
		t.Fatal("EXISTS update snapshot is missing StatusMessages")
	}

	s.WriteString("* 3 RECENT\r\n")
	recentUpdate := nextMailboxUpdate(t, updates)

	if _, ok := old.Items[imap.StatusRecent]; ok {
		t.Fatal("RECENT update added StatusRecent to the status returned by Select")
	}
	if _, ok := existsUpdate.Mailbox.Items[imap.StatusRecent]; ok {
		t.Fatal("RECENT update added StatusRecent to the previous update snapshot")
	}
	if _, ok := recentUpdate.Mailbox.Items[imap.StatusRecent]; !ok {
		t.Fatal("RECENT update snapshot is missing StatusRecent")
	}
}

func TestClient_SetStateMailboxIsImmutable(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	mbox := imap.NewMailboxStatus("INBOX", []imap.StatusItem{imap.StatusMessages})
	c.SetState(imap.SelectedState, mbox)

	updates := make(chan Update, 4)
	c.Updates = updates

	s.WriteString("* 42 EXISTS\r\n")
	update := nextMailboxUpdate(t, updates)

	if mbox.Messages != 0 {
		t.Fatalf("SetState mailbox changed in place: Messages = %d, want 0", mbox.Messages)
	}
	if update.Mailbox == mbox {
		t.Fatal("unilateral update reused the pointer supplied to SetState")
	}
	if update.Mailbox.Messages != 42 {
		t.Fatalf("update Messages = %d, want 42", update.Mailbox.Messages)
	}
	if latest := c.Mailbox(); latest != update.Mailbox {
		t.Fatal("Client.Mailbox() did not return the update snapshot")
	}
}

func TestCloneMailboxStatusDeepCopiesMutableFields(t *testing.T) {
	src := &imap.MailboxStatus{
		Name:           "INBOX",
		ReadOnly:       true,
		Items:          map[imap.StatusItem]interface{}{imap.StatusMessages: nil},
		Flags:          []string{imap.SeenFlag},
		PermanentFlags: []string{imap.DeletedFlag},
		UnseenSeqNum:   1,
		Messages:       2,
		Recent:         3,
		Unseen:         4,
		UidNext:        5,
		UidValidity:    6,
		AppendLimit:    7,
	}

	clone := cloneMailboxStatus(src)
	if clone == src {
		t.Fatal("cloneMailboxStatus returned the source pointer")
	}

	if clone.Name != src.Name || clone.ReadOnly != src.ReadOnly ||
		clone.UnseenSeqNum != src.UnseenSeqNum || clone.Messages != src.Messages ||
		clone.Recent != src.Recent || clone.Unseen != src.Unseen ||
		clone.UidNext != src.UidNext || clone.UidValidity != src.UidValidity ||
		clone.AppendLimit != src.AppendLimit {
		t.Fatalf("cloneMailboxStatus lost scalar fields: got %#v, want %#v", clone, src)
	}

	clone.Flags[0] = imap.AnsweredFlag
	clone.PermanentFlags[0] = imap.FlaggedFlag
	clone.Items[imap.StatusRecent] = nil
	delete(clone.Items, imap.StatusMessages)

	if src.Flags[0] != imap.SeenFlag {
		t.Fatalf("Flags slice is aliased: source flag = %q", src.Flags[0])
	}
	if src.PermanentFlags[0] != imap.DeletedFlag {
		t.Fatalf("PermanentFlags slice is aliased: source flag = %q", src.PermanentFlags[0])
	}
	if _, ok := src.Items[imap.StatusRecent]; ok {
		t.Fatal("Items map is aliased: clone added a key to the source")
	}
	if _, ok := src.Items[imap.StatusMessages]; !ok {
		t.Fatal("Items map is aliased: clone deleted a key from the source")
	}

	if clone.ItemsLocker.TryLock() {
		clone.ItemsLocker.Unlock()
	} else {
		t.Fatal("clone ItemsLocker is locked")
	}
	if src.ItemsLocker.TryLock() {
		src.ItemsLocker.Unlock()
	} else {
		t.Fatal("source ItemsLocker is locked")
	}

	if cloneMailboxStatus(nil) != nil {
		t.Fatal("cloneMailboxStatus(nil) is not nil")
	}
}

func TestClient_MailboxStatusConcurrentReadsUpdatesAndDisconnect(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	setClientState(c, imap.AuthenticatedState, nil)
	old := selectForSnapshotTest(t, c, s, false, "", " OK SELECT completed\r\n")

	updates := make(chan Update, 64)
	c.Updates = updates

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}

			if latest := c.Mailbox(); latest != nil {
				_ = latest.Messages
				_ = latest.Recent
				_ = latest.ReadOnly
				_ = len(latest.Flags)
				_ = len(latest.Items)
			}

			_ = old.Messages
			_ = old.Recent
			_ = len(old.Flags)
			_ = len(old.Items)
		}
	}()

	for i := 1; i <= 64; i++ {
		if _, err := fmt.Fprintf(s.Conn, "* %d EXISTS\r\n", i); err != nil {
			t.Fatalf("server write: %v", err)
		}
		update := nextMailboxUpdate(t, updates)
		if update.Mailbox.Messages != uint32(i) {
			t.Fatalf("update %d Messages = %d", i, update.Mailbox.Messages)
		}
	}

	if _, err := s.WriteString("* BYE done\r\n"); err != nil {
		t.Fatalf("server write: %v", err)
	}

	select {
	case <-c.LoggedOut():
	case <-time.After(5 * time.Second):
		t.Fatal("client did not observe the disconnect")
	}

	close(stop)
	wg.Wait()

	if latest := c.Mailbox(); latest != nil {
		t.Fatalf("Client.Mailbox() = %#v after disconnect, want nil", latest)
	}
	if old.Messages != 0 || old.Recent != 0 {
		t.Fatalf("returned snapshot changed during updates/disconnect: %#v", old)
	}
}
