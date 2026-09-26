// Extended DNS Errors (RFC 8914 and the IANA registry): the protocol names
// of the codes, shown next to the number like rcodes (not translated).

const NAMES: readonly string[] = [
  'Other Error',
  'Unsupported DNSKEY Algorithm',
  'Unsupported DS Digest Type',
  'Stale Answer',
  'Forged Answer',
  'DNSSEC Indeterminate',
  'DNSSEC Bogus',
  'Signature Expired',
  'Signature Not Yet Valid',
  'DNSKEY Missing',
  'RRSIGs Missing',
  'No Zone Key Bit Set',
  'NSEC Missing',
  'Cached Error',
  'Not Ready',
  'Blocked',
  'Censored',
  'Filtered',
  'Prohibited',
  'Stale NXDomain Answer',
  'Not Authoritative',
  'Not Supported',
  'No Reachable Authority',
  'Network Error',
  'Invalid Data',
  'Signature Expired before Valid',
  'Too Early',
  'Unsupported NSEC3 Iterations Value',
  'Unable to conform to policy',
  'Synthesized',
  'Invalid Query Type',
]

/** "15 (Blocked): Malicious domain" for an upstream's EDE. */
export function edeText(ede: { code: number; text: string }): string {
  const name = NAMES[ede.code]
  const head = name ? `${ede.code} (${name})` : String(ede.code)
  return ede.text ? `${head}: ${ede.text}` : head
}
