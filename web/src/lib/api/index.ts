// Public entry point of the API layer: import { api, resource, … } from '$lib/api'.

export { api, type ClientStatsOpts } from './endpoints'
export { ApiError, apiUrl, isApiError, request, setApiHooks, toApiError, type Query, type ReqOpts } from './client'
export { Resource, poll, resource, type Loader, type ResourceOptions } from './poll.svelte'
export { LiveStream, streamCache, streamQueries, type StreamOptions, type StreamState } from './sse.svelte'
export * from './types'
