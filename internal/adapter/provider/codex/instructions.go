package codex

import _ "embed"

// minimalInstructions is a terse stand-in for the real Codex system prompt. The Codex backend
// gates the subscription path partly on the `instructions` field, so two candidates exist: this
// minimal one and the full first-party prompt below. The provider sends the minimal candidate by
// default; the decision to fall back to fullInstructions is settled against the real backend once
// test credentials are available (the public references send the full prompt, so full is the
// likely final choice if the backend rejects minimal).
const minimalInstructions = "You are Codex, based on GPT-5, running as a coding agent in the Codex CLI on the user's computer."

// fullInstructionsText is the verbatim first-party Codex GPT-5 system prompt, embedded from the
// package's instructions_gpt5_codex.md. It is sourced from the public ChatMock reference and used
// as the fallback when the minimal candidate is rejected by the backend.
//
//go:embed instructions_gpt5_codex.md
var fullInstructionsText string

// instructions returns the default `instructions` value the provider sends: the minimal candidate.
func instructions() string {
	return minimalInstructions
}

// fullInstructions returns the full first-party Codex prompt, the fallback when the backend
// rejects the minimal candidate.
func fullInstructions() string {
	return fullInstructionsText
}
