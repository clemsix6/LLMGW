package alert

import "strconv"

// ObserveAttempt records one upstream attempt against its credential and model.
//
// The pair is the entity, because the embedded SDK cools a credential down per
// credential and model rather than globally.
func (t *Tracker) ObserveAttempt(provider, credentialID, model string, failed bool, status int) {
	if t.disabled() || credentialID == "" {
		return
	}

	// An upstream status is only meaningful on a failed attempt: a successful
	// usage record can still carry a residual value from a previous try.
	if !failed {
		status = 0
	}

	state, kind, observed := credentialTransition(failed, status)
	if !observed {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.transitionLocked(
		keyCredentialPrefix+credentialID+"\x00"+model,
		state,
		state == stateOK,
		kind,
		credentialSummary(kind, status),
		t.credentialFields(provider, credentialID, model, status),
	)
}

// credentialTransition classifies one attempt, reporting false when the attempt
// says nothing about the credential's own health.
//
// Client-caused failures are that case: a malformed body or an unknown model
// answers 4xx without the credential being at fault.
func credentialTransition(failed bool, status int) (string, Kind, bool) {
	if !failed {
		return stateOK, KindCredentialRecovered, true
	}

	switch {
	case status == 401 || status == 403:
		return stateUnauthorized, KindCredentialUnauthorized, true
	case status == 429:
		return stateRateLimited, KindCredentialRateLimited, true
	case status >= 500 || status <= 0:
		return stateFailing, KindCredentialFailing, true
	default:
		return "", "", false
	}
}

// credentialSummary describes the transition, saying so when the attempt
// carried no upstream status at all rather than rendering a status of zero.
func credentialSummary(kind Kind, status int) string {
	if kind == KindCredentialFailing && status <= 0 {
		return "Provider credential failing with no upstream status (transport failure)"
	}
	return kind.Title()
}

// credentialFields renders the operator-facing identity of one attempt,
// omitting the status entirely when the attempt reported none.
func (t *Tracker) credentialFields(provider, credentialID, model string, status int) []Field {
	label := t.labels[credentialID]
	fields := []Field{
		{Name: "Provider", Value: firstNonEmpty(provider, label.Provider)},
		{Name: "Credential", Value: firstNonEmpty(label.Label, credentialID)},
		{Name: "Model", Value: model},
	}

	if status > 0 {
		fields = append(fields, Field{Name: "Status", Value: strconv.Itoa(status)})
	}
	return fields
}
