package agent

import (
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
)

// MaxLiveAttachments is how many images stay in the history sent to the model,
// newest first. It sits far under Anthropic's 20-image threshold, where the
// per-side limit drops to 2000 px. The shell also uses it as its per-message
// image cap.
const MaxLiveAttachments = 4

// evictOldAttachments keeps the newest keep attachments across msgs and removes
// the rest, oldest first, appending to each message's text a stub naming how to
// get the image back. It returns how many it removed and is idempotent.
func evictOldAttachments(msgs []model.Message, keep int) int {
	removed := 0
	seen := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		n := len(msgs[i].Attachments)
		if n == 0 {
			continue
		}
		// Of this message's attachments, the newest (last) ones fill what is
		// left of the budget; the earlier ones go.
		room := max(keep-seen, 0)
		seen += n
		drop := n - room
		if drop <= 0 {
			continue
		}
		m := &msgs[i]
		for _, att := range m.Attachments[:drop] {
			m.Content += "\n[image " + att.Label() + " is no longer attached, to save context; read_file " + att.Label() + " to view it again]"
		}
		m.Attachments = append([]model.Attachment(nil), m.Attachments[drop:]...)
		if len(m.Attachments) == 0 {
			m.Attachments = nil
		}
		removed += drop
	}
	return removed
}

// attachmentRefs describes atts for events: metadata only, never bytes.
func attachmentRefs(atts []model.Attachment) []event.AttachmentRef {
	if len(atts) == 0 {
		return nil
	}
	out := make([]event.AttachmentRef, len(atts))
	for i, a := range atts {
		out[i] = event.AttachmentRef{Name: a.Name, MediaType: a.MediaType, Size: a.Size, SHA256: a.SHA256, Ref: a.Ref, Width: a.Width, Height: a.Height}
	}
	return out
}
