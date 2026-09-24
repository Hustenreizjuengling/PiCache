// Field ↔ control wiring: Field provides the control id, the ids of its help
// and error texts and the invalid state; Input, Select and Textarea pick them
// up automatically.

import { getContext, setContext } from 'svelte'

export interface FieldContext {
  readonly id: string
  readonly describedBy: string | undefined
  readonly invalid: boolean
  readonly required: boolean
}

const KEY = Symbol('picache.field')

export function setFieldContext(ctx: FieldContext): void {
  setContext(KEY, ctx)
}

export function getFieldContext(): FieldContext | undefined {
  return getContext<FieldContext | undefined>(KEY)
}
