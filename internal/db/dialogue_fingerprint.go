package db

import "slices"

// MirrorMessageFingerprintValues keeps dialogue in its own field so message
// wire changes do not add it a second time to stored mirror fingerprints.
func MirrorMessageFingerprintValues(messages []Message) ([]Message, []*string) {
	dialogue := DialogueFingerprintValues(messages)
	if dialogue == nil {
		return messages, nil
	}
	messages = slices.Clone(messages)
	for i := range messages {
		messages[i].DialogueText = nil
	}
	return messages, dialogue
}

// DialogueFingerprintValues preserves message positions and NULL versus empty
// dialogue. It returns nil when every value is NULL so callers can omit the
// field and preserve fingerprints from before dialogue was included.
func DialogueFingerprintValues(messages []Message) []*string {
	var dialogue []*string
	for i, m := range messages {
		if m.DialogueText != nil {
			if dialogue == nil {
				dialogue = make([]*string, len(messages))
			}
			dialogue[i] = m.DialogueText
		}
	}
	return dialogue
}
