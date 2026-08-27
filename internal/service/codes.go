package service

// Error codes sent in the `code` field of error responses. The real frontend
// (AI_Shell_WEB) looks up codes dynamically in the remote term.json — it does
// NOT hard-code this list, so adding a code only requires updating this file
// AND the remote term.json (via the CDN approval flow). The self-test frontend
// (web/lib/codes.ts) mirrors this list by hand for documentation.
// TestErrorCodesExported guards the backend list itself (uniqueness, non-empty).
// TestCodesMatchRemoteTermJSON guards that every code has a corresponding
// entry in the remote term.json — the cross-layer drift guard.
//
// Codes use the AW.xxxxxxxx format so the frontend can look them up in the
// remote term.json for a localized message. Once assigned, a code number is
// IMMUTABLE — changing it breaks deployed term.json entries. New codes append
// the next number and must be added to term.json (via the CDN approval flow).
const (
	CodeValidation          = "AW.10000001"
	CodeNotFound            = "AW.10000002"
	CodeDomainHasGoals      = "AW.10000003"
	CodeRuntimeHasAgents    = "AW.10000004"
	CodeAgentLeadsSquad     = "AW.10000005"
	CodeSkillSelected       = "AW.10000006"
	CodeAgentNameExists     = "AW.10000007"
	CodeSkillNameExists     = "AW.10000008"
	CodeRuntimeNameExists   = "AW.10000009"
	CodeDomainNameExists    = "AW.10000010"
	CodeGoalNotActivatable  = "AW.10000011"
	CodeGoalNotReopenable   = "AW.10000012"
	CodeGoalNotInReview     = "AW.10000013"
	CodeSquadNameExists     = "AW.10000014"
	CodeFieldRequired       = "AW.10000015"
	CodeAgentHasGoals       = "AW.10000016"
	CodeAgentHasSchedules   = "AW.10000017"
	CodeAgentInSquads       = "AW.10000018"
	CodeAgentHasRunningRuns = "AW.10000019"
	CodeAgentHandlesIssues  = "AW.10000020"
	CodeDomainHasSchedules  = "AW.10000021"
	CodeSquadHasGoals       = "AW.10000022"
	CodeSquadHasSchedules   = "AW.10000023"
	CodeSquadHandlesIssues  = "AW.10000024"
)
