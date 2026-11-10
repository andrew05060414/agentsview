<script lang="ts">
  import { m } from "../../i18n/index.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { sessionLabel, sessionLinkParams, type FrictionSessions } from "./frictionView.js";

  interface Props {
    subjectId: string;
    sessions: FrictionSessions | null | undefined;
    // Diagnostic subjects are producer identities, not sessions; they get no link.
    diagnostic?: boolean;
    ordinal?: number | null;
    dims?: string[];
  }

  let { subjectId, sessions, diagnostic = false, ordinal = null, dims = [] }: Props = $props();

  const info = $derived(sessions?.[subjectId]);
  const label = $derived(sessionLabel(subjectId, sessions));
  const params = $derived(sessionLinkParams({ message_ordinal: ordinal }));
  const meta = $derived(
    [
      info?.project,
      info?.agent,
      ordinal == null ? "" : m.friction_session_jump({ ordinal }),
      ...dims,
    ].filter(Boolean) as string[],
  );

  function open(event: MouseEvent) {
    if (
      event.defaultPrevented ||
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    ) {
      return;
    }
    event.preventDefault();
    // Route-first, as QualityPage evidence links do: App's deep-link effect
    // owns selection and hydration once the URL commits.
    router.navigateToSession(subjectId, params);
    if (ordinal != null) ui.scrollToOrdinal(ordinal, subjectId);
  }
</script>

<span class="session-ref">
  {#if diagnostic}
    <code class="identity">{subjectId}</code>
  {:else}
    <a
      class="session-link"
      href={router.buildSessionHref(subjectId, params)}
      title={ordinal == null
        ? m.friction_open_session_start({ session: label })
        : m.friction_open_session({ session: label, ordinal })}
      onclick={open}
    >
      {label}
    </a>
  {/if}
  {#if meta.length > 0}
    <span class="meta">
      {#each meta as part, i}
        {#if i > 0}<span class="sep" aria-hidden="true">·</span>{/if}
        <span class="meta-part">{part}</span>
      {/each}
    </span>
  {/if}
</span>

<style>
  .session-ref {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    column-gap: var(--space-4);
    row-gap: var(--space-1);
    min-width: 0;
    font-size: 12px;
  }
  .session-link {
    color: var(--accent-blue);
    text-decoration: none;
    max-width: 60ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .session-link:hover {
    text-decoration: underline;
  }
  .identity {
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--text-secondary);
  }
  .meta {
    color: var(--text-muted);
  }
  .sep {
    margin: 0 var(--space-2);
  }
</style>
