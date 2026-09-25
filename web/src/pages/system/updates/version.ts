// Version strings of the running binary (docs/ARCHITECTURE.md 14.2).

const DESCRIBE = /^(v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+?)?)-\d+-g[0-9a-f]{4,40}(?:-dirty)?$/

/**
 * The release a `git describe` development build is based on
 * ("v1.2.3-4-gabc1234-dirty" → "v1.2.3"); undefined for "dev", a bare
 * commit hash or a release version.
 */
export function baseVersion(version: string): string | undefined {
  return DESCRIBE.exec(version)?.[1]
}

/** A release version with a pre-release part ("v1.3.0-rc.1"). Check currentIsDevBuild first. */
export function isPrerelease(version: string): boolean {
  return /^v\d+\.\d+\.\d+-[0-9A-Za-z.-]+$/.test(version) && !DESCRIBE.test(version)
}
