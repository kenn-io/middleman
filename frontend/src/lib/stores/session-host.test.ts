import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import {
  consumeSessionFocus,
  discardSessionsWithPrefix,
  getSessionSlotElement,
  isSessionClaimed,
  isSessionMounted,
  isSessionSlotVisible,
  mountedSessions,
  noteSessionConnection,
  noteSessionDiscarded,
  noteSessionExited,
  noteSessionMounted,
  noteSessionReleased,
  onSessionExited,
  registerSessionSlot,
  registerSessionInput,
  requestSessionFocus,
  releaseSessionSlot,
  resetSessionHostForTest,
  sessionHostKey,
  sessionHostPrefix,
  sendSessionInput,
  sendSessionKey,
  sendSessionPastedInput,
  setRetainedSessionLimit,
  setSessionSlotVisible,
} from "./session-host.svelte.ts";

const agentOnA = sessionHostKey("ws-1", undefined, "agent", "2026-01-01T00:00:00Z");

function mountedSession(workspaceId: string, sessionKey: string) {
  const hostKey = sessionHostKey(workspaceId, undefined, sessionKey, "2026-01-01T00:00:00Z");
  return { hostKey, websocketPath: `/ws/${sessionKey}`, status: "running" };
}

function mountConnected(session: ReturnType<typeof mountedSession>): void {
  noteSessionMounted(session);
  noteSessionConnection(session.hostKey, true);
}

describe("session host registry", () => {
  beforeEach(() => resetSessionHostForTest());

  it("keys a session by workspace, host, session, and generation", () => {
    // Two fleet hosts can serve the same workspace id, and two workspaces can
    // both have a session called "agent": neither may share a live terminal.
    expect(agentOnA).not.toBe(sessionHostKey("ws-1", "build", "agent", "2026-01-01T00:00:00Z"));
    expect(agentOnA).not.toBe(sessionHostKey("ws-2", undefined, "agent", "2026-01-01T00:00:00Z"));
    // Nor may a relaunched session inherit the dead generation's subtree and its
    // closed socket.
    expect(agentOnA).not.toBe(sessionHostKey("ws-1", undefined, "agent", "2026-02-02T00:00:00Z"));
    expect(agentOnA).toBe(sessionHostKey("ws-1", undefined, "agent", "2026-01-01T00:00:00Z"));
  });

  it("routes composed input to the current pooled terminal", () => {
    const first = {
      send: vi.fn(() => true),
      sendPasted: vi.fn(() => true),
      sendKey: vi.fn(() => true),
    };
    const unregisterFirst = registerSessionInput(agentOnA, first);

    expect(sendSessionInput(agentOnA, "status\r")).toBe(true);
    expect(first.send).toHaveBeenCalledWith("status\r");
    expect(sendSessionPastedInput(agentOnA, "one\ntwo", "\r")).toBe(true);
    expect(first.sendPasted).toHaveBeenCalledWith("one\ntwo", "\r");
    expect(sendSessionKey(agentOnA, "ArrowUp")).toBe(true);
    expect(first.sendKey).toHaveBeenCalledWith("ArrowUp");

    const second = {
      send: vi.fn(() => true),
      sendPasted: vi.fn(() => true),
      sendKey: vi.fn(() => true),
    };
    const unregisterSecond = registerSessionInput(agentOnA, second);
    unregisterFirst();
    expect(sendSessionInput(agentOnA, "next\r")).toBe(true);
    expect(second.send).toHaveBeenCalledWith("next\r");

    unregisterSecond();
    expect(sendSessionInput(agentOnA, "ignored\r")).toBe(false);
    expect(sendSessionPastedInput(agentOnA, "ignored", "\r")).toBe(false);
    expect(sendSessionKey(agentOnA, "ArrowUp")).toBe(false);
  });

  it("keeps parts that contain the separator distinct", () => {
    // A workspace id is opaque; it must not be able to spell another key.
    expect(sessionHostKey("a/b", undefined, "agent", "g")).not.toBe(sessionHostKey("a", "b", "agent", "g"));
  });

  it("drops a deferred focus request when its session unmounts", () => {
    noteSessionMounted({ hostKey: agentOnA, websocketPath: "/ws/agent", status: "running" });
    requestSessionFocus(agentOnA);

    noteSessionDiscarded(agentOnA);

    // Nothing consumed it, so it would sit armed until something mounted under this
    // key again - a revisit, or the pane reopening for its own reasons - and take the
    // keyboard for a Focus Terminal pressed long ago somewhere else.
    noteSessionMounted({ hostKey: agentOnA, websocketPath: "/ws/agent", status: "running" });
    expect(consumeSessionFocus(agentOnA)).toBe(false);
  });

  it("distinguishes soft focus requests from explicit ones", () => {
    // Soft requests come from navigation (a detail surface switched items), and
    // the pool must be able to decline them when focus is somewhere sacred.
    requestSessionFocus(agentOnA, { soft: true });
    expect(consumeSessionFocus(agentOnA)).toBe("soft");
    expect(consumeSessionFocus(agentOnA)).toBe(false);

    requestSessionFocus(agentOnA);
    expect(consumeSessionFocus(agentOnA)).toBe("explicit");
  });

  it("supersedes a soft request's flavor along with its key", () => {
    const shellOnA = sessionHostKey("ws-1", undefined, "shell", "2026-01-01T00:00:00Z");
    requestSessionFocus(agentOnA, { soft: true });
    requestSessionFocus(shellOnA);
    // A stale soft flavor on a newer explicit request would let a sacred
    // element veto a focus the user explicitly asked for.
    expect(consumeSessionFocus(shellOnA)).toBe("explicit");
  });

  it("registers and clears one slot per session key", () => {
    const el = document.createElement("div");
    registerSessionSlot(agentOnA, el);
    expect(getSessionSlotElement(agentOnA)).toBe(el);
    expect(getSessionSlotElement(sessionHostKey("ws-1", undefined, "shell", "g"))).toBeNull();
    registerSessionSlot(agentOnA, null);
    expect(getSessionSlotElement(agentOnA)).toBeNull();
  });

  it("reports a mounted-but-hidden slot as not visible", () => {
    // An inactive tab panel keeps its slot in the DOM under visibility:hidden. A
    // terminal that stays active there fights the visible one for keystrokes.
    const el = document.createElement("div");
    registerSessionSlot(agentOnA, el);
    setSessionSlotVisible(agentOnA, el, false);
    expect(getSessionSlotElement(agentOnA)).not.toBeNull();
    expect(isSessionSlotVisible(agentOnA)).toBe(false);

    setSessionSlotVisible(agentOnA, el, true);
    expect(isSessionSlotVisible(agentOnA)).toBe(true);
  });

  it("reports a session with no slot as not visible", () => {
    expect(isSessionSlotVisible(agentOnA)).toBe(false);
    // A slot element that never registered must not be able to mark the session
    // visible and leave the terminal active in the parking area.
    setSessionSlotVisible(agentOnA, document.createElement("div"), true);
    expect(isSessionSlotVisible(agentOnA)).toBe(false);
  });

  it("clears visibility when the slot is replaced", () => {
    const first = document.createElement("div");
    registerSessionSlot(agentOnA, first);
    setSessionSlotVisible(agentOnA, first, true);
    registerSessionSlot(agentOnA, document.createElement("div"));
    // Re-registering must not resurrect the previous visibility: the new slot
    // says whether it is on screen.
    expect(isSessionSlotVisible(agentOnA)).toBe(false);
  });

  it("ignores a superseded slot releasing the key", () => {
    // Promotion mounts the destination slot and unmounts the source in one
    // flush, in whichever order Svelte picks. The departing slot's cleanup must
    // not wipe the arriving slot and leave the terminal parked.
    const source = document.createElement("div");
    const destination = document.createElement("div");
    registerSessionSlot(agentOnA, source);
    registerSessionSlot(agentOnA, destination);
    setSessionSlotVisible(agentOnA, destination, true);

    releaseSessionSlot(agentOnA, source);

    expect(getSessionSlotElement(agentOnA)).toBe(destination);
    expect(isSessionSlotVisible(agentOnA)).toBe(true);
  });

  it("ignores a superseded slot changing visibility", () => {
    const source = document.createElement("div");
    const destination = document.createElement("div");
    registerSessionSlot(agentOnA, source);
    registerSessionSlot(agentOnA, destination);
    setSessionSlotVisible(agentOnA, destination, true);

    setSessionSlotVisible(agentOnA, source, false);

    expect(isSessionSlotVisible(agentOnA)).toBe(true);
  });

  it("lets the owning slot release the key", () => {
    const el = document.createElement("div");
    registerSessionSlot(agentOnA, el);
    releaseSessionSlot(agentOnA, el);
    expect(getSessionSlotElement(agentOnA)).toBeNull();
  });

  it("tracks mounted sessions and updates a changed status in place", () => {
    noteSessionMounted({ hostKey: agentOnA, websocketPath: "/ws/agent", status: "starting" });
    noteSessionMounted({ hostKey: agentOnA, websocketPath: "/ws/agent", status: "running" });
    expect(mountedSessions()).toHaveLength(1);
    expect(mountedSessions()[0]?.status).toBe("running");
    expect(isSessionMounted(agentOnA)).toBe(true);

    noteSessionDiscarded(agentOnA);
    expect(mountedSessions()).toHaveLength(0);
    expect(isSessionMounted(agentOnA)).toBe(false);
  });

  it("drops the slot of an unmounted session", () => {
    noteSessionMounted({ hostKey: agentOnA, websocketPath: "/ws/agent", status: "running" });
    registerSessionSlot(agentOnA, document.createElement("div"));
    noteSessionDiscarded(agentOnA);
    // The terminal is gone, so a stale slot would have the pool reparenting a
    // subtree that no longer exists.
    expect(getSessionSlotElement(agentOnA)).toBeNull();
  });

  it("evicts released sessions in release order", () => {
    const first = mountedSession("ws-1", "first");
    const second = mountedSession("ws-2", "second");
    const third = mountedSession("ws-3", "third");
    setRetainedSessionLimit(2);

    for (const session of [first, second, third]) {
      mountConnected(session);
      noteSessionReleased(session.hostKey);
    }

    expect(mountedSessions().map(({ hostKey }) => hostKey)).toEqual([second.hostKey, third.hostKey]);
    expect(isSessionClaimed(second.hostKey)).toBe(false);
    expect(isSessionClaimed(third.hostKey)).toBe(false);
  });

  it("protects a pending destination from eviction while releasing the previous workspace", () => {
    const destination = mountedSession("ws-1", "agent");
    const previous = mountedSession("ws-2", "agent");
    setRetainedSessionLimit(1);
    mountConnected(destination);
    noteSessionReleased(destination.hostKey);
    mountConnected(previous);

    noteSessionReleased(previous.hostKey, sessionHostPrefix("ws-1", undefined));

    expect(mountedSessions().map(({ hostKey }) => hostKey)).toEqual([destination.hostKey]);
    expect(isSessionClaimed(destination.hostKey)).toBe(false);
  });

  it("moves a reclaimed session to the newest release position", () => {
    const first = mountedSession("ws-1", "first");
    const second = mountedSession("ws-2", "second");
    const third = mountedSession("ws-3", "third");
    setRetainedSessionLimit(2);
    mountConnected(first);
    noteSessionReleased(first.hostKey);
    mountConnected(second);
    noteSessionReleased(second.hostKey);

    noteSessionMounted(first);
    noteSessionReleased(first.hostKey);
    mountConnected(third);
    noteSessionReleased(third.hostKey);

    expect(mountedSessions().map(({ hostKey }) => hostKey)).toEqual([first.hostKey, third.hostKey]);
  });

  it("does not count claimed sessions toward the released limit", () => {
    const claimed = mountedSession("ws-1", "claimed");
    const released = mountedSession("ws-2", "released");
    setRetainedSessionLimit(1);
    mountConnected(claimed);
    mountConnected(released);

    noteSessionReleased(released.hostKey);

    expect(mountedSessions().map(({ hostKey }) => hostKey)).toEqual([claimed.hostKey, released.hostKey]);
    expect(isSessionClaimed(claimed.hostKey)).toBe(true);
  });

  it("applies a lower limit immediately and zero disables retention", () => {
    const first = mountedSession("ws-1", "first");
    const second = mountedSession("ws-2", "second");
    mountConnected(first);
    noteSessionReleased(first.hostKey);
    mountConnected(second);
    noteSessionReleased(second.hostKey);

    setRetainedSessionLimit(1);
    expect(mountedSessions().map(({ hostKey }) => hostKey)).toEqual([second.hostKey]);
    setRetainedSessionLimit(0);
    expect(mountedSessions()).toEqual([]);
  });

  it("discards immediately when released while disconnected", () => {
    const session = mountedSession("ws-1", "agent");
    noteSessionMounted(session);

    noteSessionReleased(session.hostKey);

    expect(isSessionMounted(session.hostKey)).toBe(false);
  });

  it("discards a released session on disconnect but keeps a claimed one", () => {
    const claimed = mountedSession("ws-1", "claimed");
    const released = mountedSession("ws-2", "released");
    mountConnected(claimed);
    mountConnected(released);
    noteSessionReleased(released.hostKey);

    noteSessionConnection(claimed.hostKey, false);
    noteSessionConnection(released.hostKey, false);

    expect(isSessionMounted(claimed.hostKey)).toBe(true);
    expect(isSessionMounted(released.hostKey)).toBe(false);
  });

  it("discards a session before routing its exit", () => {
    const session = mountedSession("ws-1", "agent");
    mountConnected(session);
    let mountedWhenRouted = true;
    const stopListening = onSessionExited((hostKey) => {
      if (hostKey === session.hostKey) mountedWhenRouted = isSessionMounted(hostKey);
    });

    noteSessionExited(session.hostKey, 0);
    stopListening();

    expect(mountedWhenRouted).toBe(false);
    expect(isSessionMounted(session.hostKey)).toBe(false);
  });

  it("purges every generation for one workspace and host prefix", () => {
    const local = mountedSession("ws-1", "agent");
    const localSecondGeneration = {
      ...local,
      hostKey: sessionHostKey("ws-1", undefined, "agent", "2026-02-02T00:00:00Z"),
    };
    const fleet = {
      ...local,
      hostKey: sessionHostKey("ws-1", "build", "agent", "2026-01-01T00:00:00Z"),
    };
    for (const session of [local, localSecondGeneration, fleet]) mountConnected(session);
    noteSessionReleased(localSecondGeneration.hostKey);

    discardSessionsWithPrefix(sessionHostPrefix("ws-1", undefined));

    expect(mountedSessions().map(({ hostKey }) => hostKey)).toEqual([fleet.hostKey]);
  });

  it("clears deferred focus on final discard", () => {
    const session = mountedSession("ws-1", "agent");
    noteSessionMounted(session);
    requestSessionFocus(session.hostKey);

    noteSessionDiscarded(session.hostKey);
    noteSessionMounted(session);

    expect(consumeSessionFocus(session.hostKey)).toBe(false);
  });
});
