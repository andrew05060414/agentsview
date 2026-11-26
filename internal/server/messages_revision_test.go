package server_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestMessagesHTTPPreservesDialogueProjectionStates(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "dialogue", "proj", 3)
	require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), "dialogue", []db.Message{
		{SessionID: "dialogue", Ordinal: 0, Role: "user", Content: "question", DialogueText: new("question")},
		{SessionID: "dialogue", Ordinal: 1, Role: "assistant", Content: "[Thinking] hidden"},
		{SessionID: "dialogue", Ordinal: 2, Role: "assistant", Content: "[Bash] hidden", DialogueText: new("")},
	}))

	response := te.get(t, "/api/v1/sessions/dialogue/messages?limit=20")
	assertStatus(t, response, http.StatusOK)
	list := decode[service.MessageList](t, response)
	require.Len(t, list.Messages, 3)
	assert.Equal(t, new("question"), list.Messages[0].DialogueText)
	assert.Nil(t, list.Messages[1].DialogueText)
	assert.Equal(t, new(""), list.Messages[2].DialogueText)
}

func TestMessagesExpectedRevisionReturnsSourceChanged(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "revisioned", "proj", 2)
	te.seedMessages(t, "revisioned", 2)

	first := te.get(t, "/api/v1/sessions/revisioned/messages?limit=20")
	assertStatus(t, first, http.StatusOK)
	list := decode[service.MessageList](t, first)
	require.NotEmpty(t, list.TranscriptRevision)
	require.NotEmpty(t, list.EvidenceSource)

	same := te.get(t, "/api/v1/sessions/revisioned/messages?limit=20&expected_revision="+
		url.QueryEscape(list.TranscriptRevision)+"&evidence_source="+url.QueryEscape(list.EvidenceSource))
	assertStatus(t, same, http.StatusOK)

	require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), "revisioned", []db.Message{{
		SessionID: "revisioned", Ordinal: 0, Role: "user", Content: "changed",
	}}))
	changed := te.get(t, "/api/v1/sessions/revisioned/messages?expected_revision="+
		url.QueryEscape(list.TranscriptRevision))
	assertStatus(t, changed, http.StatusConflict)
	assert.Contains(t, changed.Body.String(), "source_changed")
}
