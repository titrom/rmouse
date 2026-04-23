<script lang="ts">
  import { onMount, onDestroy, createEventDispatcher } from "svelte";
  import {
    GetClipboardHistory, RestoreClipboardItem, ClearClipboardHistory,
  } from "../wailsjs/go/main/App";
  import { EventsOn, EventsOff } from "../wailsjs/runtime/runtime";
  import { main } from "../wailsjs/go/models";

  export let open = false;

  const dispatch = createEventDispatcher<{ close: void }>();

  let items: main.ClipboardHistoryItemDTO[] = [];
  let error = "";

  async function refresh() {
    try {
      items = await GetClipboardHistory();
      error = "";
    } catch (e: any) {
      error = String(e?.message ?? e);
    }
  }

  async function pick(it: main.ClipboardHistoryItemDTO) {
    try {
      await RestoreClipboardItem(it.id);
      dispatch("close");
    } catch (e: any) {
      error = String(e?.message ?? e);
    }
  }

  async function clearAll() {
    try { await ClearClipboardHistory(); }
    catch (e: any) { error = String(e?.message ?? e); }
  }

  function onKey(e: KeyboardEvent) {
    if (!open) return;
    if (e.key === "Escape") { e.preventDefault(); dispatch("close"); }
  }

  function fmtTime(ms: number) {
    const d = new Date(ms);
    return d.toTimeString().slice(0, 8);
  }

  function fmtSize(n: number) {
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
    return `${(n / 1024 / 1024).toFixed(2)} MiB`;
  }

  $: if (open) refresh();

  onMount(() => {
    EventsOn("rmouse:clipboardHistory", refresh);
    window.addEventListener("keydown", onKey);
  });
  onDestroy(() => {
    EventsOff("rmouse:clipboardHistory");
    window.removeEventListener("keydown", onKey);
  });
</script>

{#if open}
  <div class="backdrop" on:click|self={() => dispatch("close")} role="presentation">
    <div class="panel" role="dialog" aria-label="Clipboard history">
      <header>
        <h2>Clipboard history</h2>
        <div class="hdr-actions">
          <button class="link-btn" on:click={clearAll} disabled={items.length === 0}>Clear</button>
          <button class="link-btn" on:click={() => dispatch("close")}>Close (Esc)</button>
        </div>
      </header>

      {#if error}
        <p class="error">{error}</p>
      {/if}

      {#if items.length === 0}
        <p class="empty">No clipboard snapshots yet. Copy something on any connected peer.</p>
      {:else}
        <ul class="list">
          {#each items as it (it.id)}
            <li>
              <button class="card" on:click={() => pick(it)} title="Click to copy back">
                <div class="meta">
                  <span class="kind kind-{it.kind}">{it.kind}</span>
                  <span class="origin">{it.origin || "?"}</span>
                  <span class="time">{fmtTime(it.timestamp)}</span>
                  <span class="size">{fmtSize(it.sizeBytes)}</span>
                </div>
                <div class="body">
                  {#if it.kind === "image" && it.imageBase64}
                    <img src={`data:image/png;base64,${it.imageBase64}`} alt="clipboard image" />
                  {:else}
                    <pre>{it.preview}</pre>
                  {/if}
                </div>
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed; inset: 0;
    background: rgba(0,0,0,0.55);
    display: flex; align-items: center; justify-content: center;
    z-index: 100;
  }
  .panel {
    width: min(720px, 94vw);
    max-height: 82vh;
    background: #1b1b1d;
    color: #eaeaea;
    border-radius: 10px;
    box-shadow: 0 18px 60px rgba(0,0,0,0.55);
    display: flex; flex-direction: column;
    overflow: hidden;
  }
  header {
    display: flex; align-items: center; justify-content: space-between;
    padding: 12px 16px;
    border-bottom: 1px solid #2a2a2d;
  }
  header h2 { margin: 0; font-size: 15px; font-weight: 600; }
  .hdr-actions { display: flex; gap: 8px; }
  .link-btn {
    background: transparent; border: none; color: #8ab4ff; cursor: pointer;
    padding: 4px 8px; border-radius: 4px; font-size: 13px;
  }
  .link-btn:hover:not(:disabled) { background: #2a2a2d; }
  .link-btn:disabled { opacity: 0.4; cursor: not-allowed; }
  .empty {
    padding: 32px 16px; text-align: center; color: #888;
  }
  .error {
    margin: 8px 16px; color: #f08080; font-size: 13px;
  }
  .list {
    list-style: none; margin: 0; padding: 8px;
    overflow-y: auto; flex: 1;
    display: flex; flex-direction: column; gap: 6px;
  }
  /* Inverted scrollbar for the dark history panel — the light-on-light
   * rule in global style.css is invisible on #1b1b1d. */
  .list::-webkit-scrollbar-thumb {
    background-color: rgba(255, 255, 255, 0.14);
  }
  .list::-webkit-scrollbar-thumb:hover {
    background-color: rgba(255, 255, 255, 0.28);
  }
  .card {
    width: 100%; text-align: left;
    background: #232326; border: 1px solid #2d2d31;
    border-radius: 8px; padding: 10px 12px;
    cursor: pointer; color: inherit;
    display: flex; flex-direction: column; gap: 6px;
  }
  .card:hover { background: #2a2a2e; border-color: #3a3a3f; }
  .meta {
    display: flex; gap: 10px; align-items: center;
    font-size: 11px; color: #9aa0a6;
  }
  .kind {
    padding: 1px 6px; border-radius: 3px; text-transform: uppercase;
    font-weight: 600; font-size: 10px; letter-spacing: 0.03em;
  }
  .kind-text { background: #1e4a7a; color: #cfe6ff; }
  .kind-image { background: #4a2d7a; color: #e4cfff; }
  .kind-files { background: #7a4a1e; color: #ffe4cf; }
  .origin { color: #c0c6cd; font-weight: 500; }
  .time, .size { margin-left: auto; }
  .size { margin-left: 4px; }
  .body pre {
    margin: 0; white-space: pre-wrap; word-break: break-word;
    max-height: 80px; overflow: hidden;
    font-size: 12px; line-height: 1.4; color: #d5d5d6;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  }
  .body img {
    max-height: 160px; max-width: 100%;
    object-fit: contain; display: block;
    background: #0f0f10; border-radius: 4px;
  }
</style>
