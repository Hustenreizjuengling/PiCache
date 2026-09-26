// Your rules: the editable draft of a rule, the input sent for it (only the
// options that fit its action and type, so the server resets the others)
// and short texts for the badges of the rules table and the match lists.

import { t, type MessageKey } from '$i18n/index.svelte'
import { DEFAULT_GROUP_ID, SELF_ADDRESS, type FilterRule, type FilterRuleInput, type RuleAction, type RuleReply, type RuleType } from '$lib/api'
import { asciiDomain } from '../shared/input'

/** Everything the rule panel edits. */
export interface RuleDraft {
  action: RuleAction
  type: RuleType
  pattern: string
  enabled: boolean
  groupIds: number[]
  comment: string
  qtypes: string[]
  qtypesNegate: boolean
  reply: RuleReply
  replyIpv4: string
  replyIpv6: string
  denyallow: string[]
  invert: boolean
}

export const REPLIES: readonly RuleReply[] = ['', 'null', 'nxdomain', 'nodata', 'refused', 'custom_ip']

export function blankRule(): RuleDraft {
  return {
    action: 'block',
    type: 'exact',
    pattern: '',
    enabled: true,
    groupIds: [DEFAULT_GROUP_ID],
    comment: '',
    qtypes: [],
    qtypesNegate: false,
    reply: '',
    replyIpv4: '',
    replyIpv6: '',
    denyallow: [],
    invert: false,
  }
}

/** The draft of a stored rule. */
export function draftOf(r: FilterRule): RuleDraft {
  return {
    action: r.action,
    type: r.type,
    pattern: r.pattern,
    enabled: r.enabled,
    groupIds: [...r.groupIds],
    comment: r.comment,
    qtypes: [...r.qtypes],
    qtypesNegate: r.qtypesNegate,
    reply: r.reply,
    replyIpv4: r.replyIpv4,
    replyIpv6: r.replyIpv6,
    denyallow: [...r.denyallow],
    invert: r.invert,
  }
}

/** Block rules choose their answer. */
export function hasReply(d: Pick<RuleDraft, 'action'>): boolean {
  return d.action === 'block'
}

/** Exceptions: block rules for a domain with subdomains or a regular expression. */
export function hasDenyallow(d: Pick<RuleDraft, 'action' | 'type'>): boolean {
  return d.action === 'block' && d.type !== 'exact'
}

/** Inverting: regular-expression block rules. */
export function hasInvert(d: Pick<RuleDraft, 'action' | 'type'>): boolean {
  return d.action === 'block' && d.type === 'regex'
}

/**
 * The input for a draft. Options that do not fit the action and type are
 * left out (the server resets them to their defaults); without groups
 * (device rules) the server chooses them.
 */
export function ruleInput(d: RuleDraft, withGroups = true): FilterRuleInput {
  const custom = hasReply(d) && d.reply === 'custom_ip'
  return {
    action: d.action,
    type: d.type,
    pattern: d.type === 'regex' ? d.pattern.trim() : asciiDomain(d.pattern),
    enabled: d.enabled,
    comment: d.comment.trim(),
    ...(withGroups ? { groupIds: [...d.groupIds] } : {}),
    qtypes: [...d.qtypes],
    qtypesNegate: d.qtypes.length > 0 && d.qtypesNegate,
    ...(hasReply(d)
      ? { reply: d.reply, replyIpv4: custom ? d.replyIpv4.trim() : '', replyIpv6: custom ? d.replyIpv6.trim() : '' }
      : {}),
    ...(hasDenyallow(d) ? { denyallow: d.denyallow.map(asciiDomain) } : {}),
    ...(hasInvert(d) ? { invert: d.invert } : {}),
  }
}

/** "A, AAAA" or "All but A, AAAA". */
export function typesText(qtypes: readonly string[], negate: boolean): string {
  const list = qtypes.join(', ')
  return negate ? t('dns.rules.badge.notTypes', { types: list }) : list
}

/** The name of a reply in the rule form ("Default blocking mode", "NXDOMAIN (domain does not exist)", …). */
export function replyLabel(r: RuleReply): string {
  if (r === '') return t('dns.rules.reply.default')
  if (r === 'custom_ip') return t('dns.rules.reply.custom')
  return t(`dns.settings.blocking.mode.${r}` as MessageKey)
}

/** An address of a custom reply, "self" in words. */
export function addressText(a: string): string {
  return a === SELF_ADDRESS ? t('dns.shared.selfAddressShort') : a
}

/** The short reply of a badge ("Refused", "192.168.1.2, this server"); "" for the default. */
export function replyShort(r: { reply: RuleReply; replyIpv4?: string; replyIpv6?: string }): string {
  if (!r.reply) return ''
  if (r.reply === 'custom_ip') {
    const addrs = [r.replyIpv4, r.replyIpv6].filter((a): a is string => !!a).map(addressText)
    return addrs.length > 0 ? addrs.join(', ') : t('dns.rules.reply.custom')
  }
  return t(`dns.rules.replyShort.${r.reply}` as MessageKey)
}
