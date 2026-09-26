// What devices enter for PiCache's encrypted DNS: the DoT host (a ClientID
// goes in front of the server name, as the TLS name) and the DoH address (a
// ClientID is a last path segment), plus the checks of the Apple profile
// options that the server repeats.

/** The only DoT port Android's Private DNS and Apple devices use. */
export const DOT_PORT = 853

/** Longest link the QR encoder takes (byte mode, level M, version 10). */
export const QR_MAX_BYTES = 213

/** Most Wi-Fi names in a profile, and their length in bytes. */
export const MAX_SSIDS = 16
export const MAX_SSID_BYTES = 32

/**
 * The DoT port devices use: 853 while a DoT listener is bound there (or none
 * is bound), else the port of the first listener. `port` is only the first
 * listener's, so ":8853,:853" still serves Android and Apple devices on 853.
 */
export function dotDevicePort(dot: { port?: number; listeners: readonly string[] }): number {
  if (!dot.port || dot.listeners.some((a) => a.endsWith(`:${DOT_PORT}`))) return DOT_PORT
  return dot.port
}

/** The DoT host for display: "dns.example.com", with ":<port>" when the port is not 853. */
export function dotHost(host: string, port?: number): string {
  return port && port !== DOT_PORT ? `${host}:${port}` : host
}

/** The TLS name that carries a ClientID: "<ClientID>.<server name>". */
export function withClientIdHost(host: string, clientId: string): string {
  return clientId ? `${clientId}.${host}` : host
}

/** The DoH address with a ClientID: "https://…/dns-query/<ClientID>". */
export function withClientIdUrl(url: string, clientId: string): string {
  return clientId ? `${url.replace(/\/+$/, '')}/${clientId}` : url
}

/** Bytes of a string in UTF-8. */
export function utf8Length(s: string): number {
  return new TextEncoder().encode(s).length
}

/** Whether the Wi-Fi names fit a profile: at most 16, each 1–32 bytes without control characters. */
export function validSsids(ssids: readonly string[]): boolean {
  return (
    ssids.length <= MAX_SSIDS &&
    ssids.every((s) => {
      const n = utf8Length(s)
      return n >= 1 && n <= MAX_SSID_BYTES && !/[\u0000-\u001f\u007f-\u009f]/.test(s)
    })
  )
}
