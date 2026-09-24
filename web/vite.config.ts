import { writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { defineConfig, type Plugin } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// The Go embed directory keeps a .gitkeep so `go build` works before the UI
// was built; emptyOutDir removes it, so it is written back after each build.
const keepGitkeep: Plugin = {
  name: 'picache-keep-gitkeep',
  apply: 'build',
  writeBundle(options) {
    if (options.dir) writeFileSync(join(options.dir, '.gitkeep'), '')
  },
}

export default defineConfig({
  plugins: [svelte(), keepGitkeep],
  base: './', // relative asset URLs: works under any mount path
  resolve: {
    alias: {
      $lib: fileURLToPath(new URL('./src/lib', import.meta.url)),
      $i18n: fileURLToPath(new URL('./src/i18n', import.meta.url)),
    },
  },
  build: {
    outDir: '../internal/webui/dist', // embedded by internal/webui (go:embed all:dist)
    emptyOutDir: true,
    assetsDir: 'assets',
    sourcemap: false,
    target: 'es2022',
    modulePreload: { polyfill: false }, // modern browsers only; never an inline script
    chunkSizeWarningLimit: 600,
  },
  server: {
    proxy: { '/api': 'http://127.0.0.1:8080' }, // Go backend during `npm run dev`
  },
})
