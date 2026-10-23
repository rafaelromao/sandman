package batch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rafaelromao/sandman/internal/atomicfs"
	"github.com/rafaelromao/sandman/internal/daemon"
	"github.com/rafaelromao/sandman/internal/paths"
)

// agentSessionFile is the Run artifact that records the agent session
// identity: <batchDir>/runs/<runID>/session.json.
const agentSessionFile = "session.json"

// agentSessionKind is the protocol and provider one agent strategy records in
// a Run's session.json. Each strategy validates only its own kind, so a record
// written by another agent is never selected for a launch.
type agentSessionKind struct {
	protocol string
	provider string
}

type agentSessionIdentity struct {
	Protocol  string `json:"protocol"`
	Provider  string `json:"provider"`
	SessionID string `json:"session_id"`
}

func (k agentSessionKind) validate(id agentSessionIdentity) error {
	if id.Protocol != k.protocol {
		return fmt.Errorf("unsupported session protocol %q", id.Protocol)
	}
	if id.Provider != k.provider {
		return fmt.Errorf("unsupported session provider %q", id.Provider)
	}
	if strings.TrimSpace(id.SessionID) == "" {
		return errors.New("session id is empty")
	}
	return nil
}

func decodeAgentSession(path string) (agentSessionIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return agentSessionIdentity{}, err
	}
	var identity agentSessionIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return agentSessionIdentity{}, fmt.Errorf("parse session metadata: %w", err)
	}
	return identity, nil
}

func readAgentSession(path string, kind agentSessionKind) (agentSessionIdentity, error) {
	identity, err := decodeAgentSession(path)
	if err != nil {
		return agentSessionIdentity{}, err
	}
	if err := kind.validate(identity); err != nil {
		return agentSessionIdentity{}, err
	}
	return identity, nil
}

// priorAgentSession selects canonical metadata first. Legacy lookup is
// deliberately limited to a missing canonical file so a broken canonical
// record cannot be silently replaced by stale evidence. A Run recorded
// without a batch lives in the batch its RunID encodes, as the Run folder does.
// That Batch's frozen location comes from the index; a Batch the index cannot
// locate is an error rather than a guessed directory.
func priorAgentSession(layout paths.Layout, batchID, runID string, kind agentSessionKind) (agentSessionIdentity, bool, error) {
	if batchID == "" {
		batchID = batchIDFromRunID(runID)
	}
	batch, err := daemon.ResolveBatchLocation(layout, batchID)
	if err != nil {
		return agentSessionIdentity{}, false, err
	}
	identity, err := readAgentSession(filepath.Join(batch.Run(runID).Dir, agentSessionFile), kind)
	if err == nil {
		return identity, true, nil
	}
	if !os.IsNotExist(err) {
		return agentSessionIdentity{}, false, err
	}
	identity, err = readAgentSession(layout.LegacyRunSessionPath(runID), kind)
	if err == nil {
		return identity, true, nil
	}
	if os.IsNotExist(err) {
		return agentSessionIdentity{}, false, nil
	}
	return agentSessionIdentity{}, false, err
}

func writeAgentSession(path string, kind agentSessionKind, sessionID string) error {
	identity := agentSessionIdentity{
		Protocol:  kind.protocol,
		Provider:  kind.provider,
		SessionID: strings.TrimSpace(sessionID),
	}
	if err := kind.validate(identity); err != nil {
		return err
	}
	return atomicfs.WriteAtomicJSON(path, identity, 0644)
}

// agentSessionProtocolPattern is the shape every strategy's session protocol
// shares, such as "opencode-session/v1".
var agentSessionProtocolPattern = regexp.MustCompile(`^[a-z]+-session/v[0-9]+$`)

// ReadAgentSessionID returns the agent session ID recorded in a Run folder's
// session.json, whichever agent wrote it, for display in Run details. A
// missing or malformed record reports false.
func ReadAgentSessionID(runDir string) (string, bool) {
	if strings.TrimSpace(runDir) == "" {
		return "", false
	}
	identity, err := decodeAgentSession(filepath.Join(runDir, agentSessionFile))
	if err != nil || !agentSessionProtocolPattern.MatchString(identity.Protocol) || identity.Provider == "" {
		return "", false
	}
	sessionID := strings.TrimSpace(identity.SessionID)
	return sessionID, sessionID != ""
}
