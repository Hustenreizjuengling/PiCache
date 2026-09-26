// Account management (System > Users & security): the actions of the users
// table and the server's rules for accounts (internal/auth), mirrored so
// mistakes show up next to the field before anything is sent.

/** What the user dialog does: add an account or change or delete one. */
export type UserAction = 'create' | 'role' | 'password' | 'totp' | 'delete'

/** Most accounts the server allows. */
export const MAX_ACCOUNTS = 32

/** 1–64 letters, digits and . _ @ -, starting with a letter or digit (unique regardless of case). */
export const USERNAME = /^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$/
