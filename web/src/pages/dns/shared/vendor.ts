// The manufacturer of a MAC address as the server reports it (IEEE registry;
// omitted when unknown). A locally administered address (a phone's private
// Wi-Fi address) never has one: the server marks it macRandomized instead,
// and the UI says so.

import { t } from '$i18n/index.svelte'

/** Anything the server annotates with vendor information (seen addresses, devices, leases, reservations). */
export interface VendorInfo {
  vendor?: string
  macRandomized?: boolean
}

/** The manufacturer, "Private address (randomised)", or undefined when neither is known. */
export function vendorText(v: VendorInfo | undefined): string | undefined {
  if (!v) return undefined
  if (v.vendor) return v.vendor
  return v.macRandomized ? t('dns.vendor.private') : undefined
}

/** The distinct vendor texts of several rows (e.g. every address of a client), in order. */
export function vendorTexts(rows: readonly VendorInfo[]): string[] {
  const out: string[] = []
  for (const r of rows) {
    const v = vendorText(r)
    if (v && !out.includes(v)) out.push(v)
  }
  return out
}
