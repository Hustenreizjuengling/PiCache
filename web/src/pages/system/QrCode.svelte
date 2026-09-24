<!--
  @component
  A QR code for `text`, encoded in the browser (./qr.ts) and drawn as SVG.
  Always dark on white with a quiet zone, which scanners need in both themes.
  Shows `fallback` when the text is too long for a QR code.
-->
<script lang="ts">
  import { encodeQr, qrPath } from './qr'

  interface Props {
    text: string
    /** Accessible name of the image. */
    label: string
    /** Shown instead of the code when the text does not fit. */
    fallback: string
    /** Rendered width and height in CSS pixels. */
    size?: number
  }

  let { text, label, fallback, size = 208 }: Props = $props()

  const MARGIN = 4 // quiet zone in modules (ISO/IEC 18004)
  const qr = $derived(encodeQr(text))
  const box = $derived(qr ? qr.size + 2 * MARGIN : 0)
</script>

{#if qr}
  <svg
    class="qr"
    role="img"
    aria-label={label}
    viewBox="0 0 {box} {box}"
    width={size}
    height={size}
    shape-rendering="crispEdges"
  >
    <rect width={box} height={box} fill="#ffffff" />
    <path d={qrPath(qr, MARGIN)} fill="#000000" />
  </svg>
{:else}
  <p class="small muted">{fallback}</p>
{/if}

<style>
  .qr {
    display: block;
    flex: none;
    max-width: 100%;
    height: auto;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
</style>
