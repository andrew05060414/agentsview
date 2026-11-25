// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { initI18n, m } from "../../i18n/index.js";
import { ui } from "../../stores/ui.svelte.js";

const sessionState = vi.hoisted(() => ({
  sessions: {
    activeSessionId: "session-1",
    activeSession: {
      id: "session-1",
      display_name: "My chat session",
      first_message: null,
      project: null,
    },
    deleteSession: vi.fn(),
  },
}));

vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: sessionState.sessions,
}));

// @ts-ignore
import ConfirmDeleteModal from "./ConfirmDeleteModal.svelte";

async function settle(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
  await tick();
}

function button(label: string): HTMLButtonElement {
  const el = [...document.body.querySelectorAll("button")].find(
    (b) => b.textContent?.trim() === label,
  );
  if (!el) throw new Error(`button not found: ${label}`);
  return el as HTMLButtonElement;
}

describe("ConfirmDeleteModal", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    localStorage.clear();
    initI18n();
    sessionState.sessions.deleteSession.mockReset();
    sessionState.sessions.deleteSession.mockResolvedValue(undefined);
    sessionState.sessions.activeSessionId = "session-1";
    sessionState.sessions.activeSession = {
      id: "session-1",
      display_name: "My chat session",
      first_message: null,
      project: null,
    };
    ui.activeModal = null;
    document.body.innerHTML = "";
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    ui.activeModal = null;
    document.body.innerHTML = "";
  });

  it("names the session being deleted", async () => {
    component = mount(ConfirmDeleteModal, { target: document.body });
    await settle();
    expect(document.body.querySelector(".confirm-message")?.textContent).toBe(
      m.confirm_delete_message({ name: "My chat session" }),
    );
  });

  it("focuses the delete action when the modal opens", async () => {
    component = mount(ConfirmDeleteModal, { target: document.body });
    await settle();
    await settle();
    const deleteButton = document.body.querySelector(".confirm-delete-action") as HTMLElement;
    expect(deleteButton).not.toBeNull();
    expect(document.activeElement).toBe(deleteButton);
  });

  it("cancel closes without deleting", async () => {
    ui.activeModal = "confirmDelete";
    component = mount(ConfirmDeleteModal, { target: document.body });
    await settle();

    button(m.confirm_delete_cancel()).click();
    await settle();

    expect(sessionState.sessions.deleteSession).not.toHaveBeenCalled();
    expect(ui.activeModal).toBeNull();
  });

  it("confirm deletes the active session exactly once and closes", async () => {
    ui.activeModal = "confirmDelete";
    component = mount(ConfirmDeleteModal, { target: document.body });
    await settle();

    button(m.confirm_delete_move_to_trash()).click();
    await settle();

    expect(sessionState.sessions.deleteSession).toHaveBeenCalledExactlyOnceWith("session-1");
    expect(ui.activeModal).toBeNull();
  });
});
