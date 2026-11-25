// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, waitFor } from "@testing-library/svelte";
import { initI18n, m } from "../../i18n/index.js";
import { settings } from "../../stores/settings.svelte.js";

const services = vi.hoisted(() => ({
  postApiV1ConfigGithub: vi.fn(),
}));

vi.mock("../../api/generated/index", () => ({
  ConfigService: {
    postApiV1ConfigGithub: services.postApiV1ConfigGithub,
  },
}));

// @ts-ignore
import GithubSettings from "./GithubSettings.svelte";

function tokenInput(): HTMLInputElement {
  const el = document.body.querySelector<HTMLInputElement>('input[type="password"]');
  if (!el) throw new Error("token input not rendered");
  return el;
}

beforeEach(() => {
  localStorage.clear();
  initI18n();
  services.postApiV1ConfigGithub.mockReset();
  services.postApiV1ConfigGithub.mockResolvedValue({});
  settings.saving = false;
  settings.githubConfigured = false;
  vi.spyOn(settings, "load").mockResolvedValue();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  localStorage.clear();
});

describe("GithubSettings", () => {
  it("keeps Save disabled on an empty or blank token", async () => {
    const { getByRole } = render(GithubSettings);
    const save = getByRole("button", {
      name: m.settings_github_save_token(),
    }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);

    await fireEvent.input(tokenInput(), { target: { value: "   " } });
    expect(save.disabled).toBe(true);
    await fireEvent.click(save);
    expect(services.postApiV1ConfigGithub).not.toHaveBeenCalled();
  });

  it("posts the trimmed token, clears the field, and shows success", async () => {
    const { getByRole } = render(GithubSettings);
    await fireEvent.input(tokenInput(), { target: { value: "  ghp_test123  " } });
    await fireEvent.click(getByRole("button", { name: m.settings_github_save_token() }));

    await waitFor(() => {
      expect(document.body.querySelector(".msg.success")?.textContent).toBe(
        m.settings_github_token_saved(),
      );
    });
    expect(services.postApiV1ConfigGithub).toHaveBeenCalledExactlyOnceWith({
      token: "ghp_test123",
    });
    expect(tokenInput().value).toBe("");
    expect(document.body.querySelector(".msg.error")).toBeNull();
  });

  it("shows the failure and no success state when saving errors", async () => {
    services.postApiV1ConfigGithub.mockRejectedValue(new Error("forbidden"));
    const { getByRole } = render(GithubSettings);
    await fireEvent.input(tokenInput(), { target: { value: "ghp_bad" } });
    await fireEvent.click(getByRole("button", { name: m.settings_github_save_token() }));

    await waitFor(() => {
      expect(document.body.querySelector(".msg.error")?.textContent).toBe("forbidden");
    });
    expect(document.body.querySelector(".msg.success")).toBeNull();
    expect(settings.load).not.toHaveBeenCalled();
  });
});
