package client

import "github.com/flowbyte-com/go-imap"

// cloneMailboxStatus returns an independent snapshot of status.
//
// MailboxStatus contains a mutex, so it must not be copied with a struct
// assignment. The fields are copied explicitly here and the clone gets its own
// zero ItemsLocker. Flags, PermanentFlags and the Items map are copied so that
// later changes to either status cannot affect the other. The values stored in
// Items are treated as immutable, which matches the MailboxStatus contract
// that they must only be used by protocol-extension libraries.
func cloneMailboxStatus(status *imap.MailboxStatus) *imap.MailboxStatus {
	if status == nil {
		return nil
	}

	clone := &imap.MailboxStatus{
		Name:         status.Name,
		ReadOnly:     status.ReadOnly,
		UnseenSeqNum: status.UnseenSeqNum,
		Messages:     status.Messages,
		Recent:       status.Recent,
		Unseen:       status.Unseen,
		UidNext:      status.UidNext,
		UidValidity:  status.UidValidity,
		AppendLimit:  status.AppendLimit,
	}

	status.ItemsLocker.Lock()
	if status.Items != nil {
		clone.Items = make(map[imap.StatusItem]interface{}, len(status.Items))
		for item, value := range status.Items {
			clone.Items[item] = value
		}
	}
	status.ItemsLocker.Unlock()

	if status.Flags != nil {
		clone.Flags = append([]string(nil), status.Flags...)
	}
	if status.PermanentFlags != nil {
		clone.PermanentFlags = append([]string(nil), status.PermanentFlags...)
	}

	return clone
}
