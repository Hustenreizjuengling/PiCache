// A short, readable description of a User-Agent header for the sessions
// table ("Firefox", "Windows"). The full string stays available in the
// details; this only has to be good enough to recognise one's own devices.

export interface Device {
  browser: string
  os: string
}

const BROWSERS: [RegExp, string][] = [
  [/\bEdg(?:e|A|iOS)?\//, 'Edge'],
  [/\b(?:OPR|Opera)\//, 'Opera'],
  [/\bVivaldi\//, 'Vivaldi'],
  [/\bSamsungBrowser\//, 'Samsung Internet'],
  [/\b(?:Firefox|FxiOS)\//, 'Firefox'],
  [/(?:Chrome|CriOS|Chromium)\//, 'Chrome'], // also matches HeadlessChrome
  [/\bVersion\/[\d.]+.*\bSafari\//, 'Safari'],
  [/^curl\//, 'curl'],
]

const SYSTEMS: [RegExp, string][] = [
  [/\bWindows\b/, 'Windows'],
  [/\bAndroid\b/, 'Android'],
  [/\biPad\b/, 'iPadOS'],
  [/\b(?:iPhone|iPod)\b/, 'iOS'],
  [/\bCrOS\b/, 'ChromeOS'],
  [/\bMac OS X\b|\bMacintosh\b/, 'macOS'],
  [/\bLinux\b|\bX11\b/, 'Linux'],
]

function first(list: [RegExp, string][], ua: string): string {
  for (const [re, name] of list) if (re.test(ua)) return name
  return ''
}

/** Browser and operating system named in a User-Agent ('' when unknown). */
export function describeUserAgent(ua: string): Device {
  return { browser: first(BROWSERS, ua), os: first(SYSTEMS, ua) }
}
