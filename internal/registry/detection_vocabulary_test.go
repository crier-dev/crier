package registry

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInboxAcceptanceDetectorVocabulary(t *testing.T) {
	verdict, transport := deliveryVerdict(http.StatusCreated, "")
	require.Equal(t, "inbox_accepted", verdict, "storing a message is not observing an answer")
	require.Equal(t, "inbox", transport)
	require.Equal(t, transport, transportForVerdict(verdict))
	// Explicit overrides must retain the same transport mapping too.
	overrideVerdict, overrideTransport := deliveryVerdict(http.StatusOK, verdict)
	require.Equal(t, verdict, overrideVerdict)
	require.Equal(t, transport, overrideTransport)
}
