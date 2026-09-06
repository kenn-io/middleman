const storageKey = "kenn-forge:show-list-agent-status";

function readPreference(): boolean {
  try {
    return localStorage.getItem(storageKey) === "true";
  } catch {
    return false;
  }
}

let enabled = $state(readPreference());

export function getShowListAgentStatus(): boolean {
  return enabled;
}

export function setShowListAgentStatus(value: boolean): void {
  enabled = value;
  try {
    localStorage.setItem(storageKey, String(value));
  } catch {
    // Keep the preference for this session when browser storage is unavailable.
  }
}
