// A rough password strength hint (0 = too short … 4 = strong). It only
// guides the user; the server enforces the minimum length.

const COMMON = /^(?:password|passwort|picache|admin|qwert|azert|letmein|welcome|123456|abc123)/i

export function passwordStrength(pw: string, min: number): 0 | 1 | 2 | 3 | 4 {
  const length = [...pw].length // characters, as the server counts them
  if (length < min) return 0
  if (COMMON.test(pw) || /^(.)\1+$/.test(pw) || /^\d+$/.test(pw)) return 1
  const classes = [/[a-z]/, /[A-Z]/, /\d/, /[^A-Za-z0-9]/].filter((r) => r.test(pw)).length
  const unique = new Set(pw).size
  let score = 1
  if (length >= 14) score++
  if (length >= 20) score++
  if (classes >= 3) score++
  if (unique < 6) score = Math.min(score, 2)
  return Math.min(4, score) as 1 | 2 | 3 | 4
}
