// Frontend mirror of internal/service/codes.go. There is no test that
// asserts this list matches the backend's, so a new backend code requires
// a manual addition here. Codes use the AW.xxxxxxxx format so the real
// frontend (AI_Shell_WEB) can look them up in the remote term.json for a
// localized message. The self-test web/ only verifies the backend outputs
// the correct code/detail — it does not query term.json.
export const CodeValidation = "AW.10000001" as const;
export const CodeNotFound = "AW.10000002" as const;
export const CodeDomainHasGoals = "AW.10000003" as const;
export const CodeRuntimeHasAgents = "AW.10000004" as const;
export const CodeAgentLeadsSquad = "AW.10000005" as const;
export const CodeSkillSelected = "AW.10000006" as const;
export const CodeAgentNameExists = "AW.10000007" as const;
export const CodeSkillNameExists = "AW.10000008" as const;
export const CodeRuntimeNameExists = "AW.10000009" as const;
export const CodeDomainNameExists = "AW.10000010" as const;
export const CodeGoalNotActivatable = "AW.10000011" as const;
export const CodeGoalNotReopenable = "AW.10000012" as const;
export const CodeGoalNotInReview = "AW.10000013" as const;
export const CodeSquadNameExists = "AW.10000014" as const;
export const CodeFieldRequired = "AW.10000015" as const;
export const CodeAgentHasGoals = "AW.10000016" as const;
export const CodeAgentHasSchedules = "AW.10000017" as const;
export const CodeAgentInSquads = "AW.10000018" as const;
export const CodeAgentHasRunningRuns = "AW.10000019" as const;
export const CodeAgentHandlesIssues = "AW.10000020" as const;
export const CodeDomainHasSchedules = "AW.10000021" as const;
export const CodeSquadHasGoals = "AW.10000022" as const;
export const CodeSquadHasSchedules = "AW.10000023" as const;
export const CodeSquadHandlesIssues = "AW.10000024" as const;

// The complete set — hand-mirrored from internal/service/codes.go.
export const ALL_CODES = [
  CodeValidation,
  CodeNotFound,
  CodeDomainHasGoals,
  CodeRuntimeHasAgents,
  CodeAgentLeadsSquad,
  CodeSkillSelected,
  CodeAgentNameExists,
  CodeSkillNameExists,
  CodeRuntimeNameExists,
  CodeDomainNameExists,
  CodeGoalNotActivatable,
  CodeGoalNotReopenable,
  CodeGoalNotInReview,
  CodeSquadNameExists,
  CodeFieldRequired,
  CodeAgentHasGoals,
  CodeAgentHasSchedules,
  CodeAgentInSquads,
  CodeAgentHasRunningRuns,
  CodeAgentHandlesIssues,
  CodeDomainHasSchedules,
  CodeSquadHasGoals,
  CodeSquadHasSchedules,
  CodeSquadHandlesIssues,
] as const;
