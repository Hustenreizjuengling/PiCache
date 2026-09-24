// Übersicht.

import type en from '../en/overview'
import type { Messages } from '../types'

const de: Messages<typeof en> = {
  loadError: 'Der Status konnte nicht geladen werden',
  rangeLabel: 'Zeitraum der Diagramme und Top-Listen',
  'unit.qpm': 'Anfragen/Min.',
  'unit.perMinute': '{n}/Min.',

  'sentence.dns': 'DNS beantwortet {rate}',
  'sentence.blocked': '{share} in den letzten 24 Stunden blockiert',
  'sentence.cache': 'der Cache hat in den letzten 24 Stunden {bytes} ausgeliefert, {ratio} davon von der Platte',
  'sentence.cacheIdle': 'der Cache hat in den letzten 24 Stunden nichts ausgeliefert',
  'sentence.cacheOff': 'LanCache ist aus',
  'sentence.paused': 'Blockieren ist bis {time} pausiert',
  'sentence.blockingOff': 'Blockieren ist aus',

  'health.title.one': '{count} Zustandsprüfung braucht deine Aufmerksamkeit',
  'health.title.other': '{count} Zustandsprüfungen brauchen deine Aufmerksamkeit',
  'health.more.one': '… und {count} weitere',
  'health.more.other': '… und {count} weitere',
  'health.open': 'Zustand & Info öffnen',

  'dns.title': 'DNS',
  'dns.openLog': 'Abfrageprotokoll öffnen',
  'dns.chartLabel': 'DNS-Anfragen pro Minute, erlaubt und blockiert',
  'dns.allowed': 'Erlaubt',
  'dns.blocked': 'Blockiert',
  'dns.topBlocked': 'Am häufigsten blockierte Domains',
  'dns.topClients': 'Aktivste Clients',
  'dns.topSince': 'seit {time}',
  'dns.topSinceHint': 'Die Listen werden pro Stunde gezählt und enthalten daher alles seit Beginn der Stunde, in der der Zeitraum beginnt.',
  'dns.domain': 'Domain',
  'dns.client': 'Client',
  'dns.queries': 'Anfragen',
  'dns.noBlocked': 'In diesem Zeitraum wurde nichts blockiert.',
  'dns.noClients':
    'Keine Anfragen in diesem Zeitraum. Trage PiCache als DNS-Server in deinem Router oder auf deinen Geräten ein.',

  'cache.title': 'Cache',
  'cache.openDownloads': 'Downloads öffnen',
  'cache.chartLabel': 'Download-Durchsatz aus dem Cache und aus dem Internet',
  'cache.hit': 'Aus dem Cache',
  'cache.wan': 'Aus dem Internet',
  'cache.notReady': 'LanCache ist an, aber Download-Hosts werden nicht umgeleitet',
  'cache.notReadyText': 'PiCache hat keine nutzbare Cache-IP-Adresse. Lege sie in den Cache-Einstellungen fest.',
  'cache.openSettings': 'Cache-Einstellungen öffnen',
  'cache.servicesNotReady': 'Die Liste der Download-Dienste ist noch nicht geladen',
  'cache.servicesNotReadyText':
    'PiCache lädt sie von uklans/cache-domains; bis dahin wird nichts gecacht. Wenn das so bleibt, prüfe die Internetverbindung.',

  'live.title': 'Läuft gerade',
  'live.content': 'Inhalt',
  'live.client': 'Client',
  'live.speed': 'Tempo',
  'live.fromCache': 'Aus dem Cache',
  'live.sent': 'Übertragen',
  'live.empty': 'Gerade laufen keine Downloads',
  'live.emptyText': 'Stelle den DNS eines Clients auf PiCache um und starte einen Steam-Download.',

  'storage.title': 'Speicher',
  'storage.summary': '{cached} gecacht · {free} frei von {total}',
  'storage.cachedOnly': '{cached} gecacht · freier Platz unbekannt',
  'storage.cached': 'Gecacht',
  'storage.other': 'Andere Dateien',
  'storage.free': 'Frei',
  'storage.minFree': 'Alte Inhalte werden entfernt, wenn weniger als {size} frei sind',
  'storage.fullIn.one': 'Bei der aktuellen Rate ist der Cache in etwa {count} Tag voll',
  'storage.fullIn.other': 'Bei der aktuellen Rate ist der Cache in etwa {count} Tagen voll',
  'storage.limitIn.one': 'Bei der aktuellen Rate erreicht der Cache in etwa {count} Tag sein Größenlimit',
  'storage.limitIn.other': 'Bei der aktuellen Rate erreicht der Cache in etwa {count} Tagen sein Größenlimit',
  'storage.atLimit': 'Größenlimit ({size}) erreicht: alte Inhalte werden entfernt',
  'storage.full': 'Voll: neue Downloads werden nicht gecacht',
  'storage.low': 'Wenig Platz: alte Inhalte werden entfernt',
  'storage.sdCard': 'Auf einer SD-Karte: langsam und verschleißt',
  'storage.manage': 'Speicher verwalten',
  'storage.offline': 'Der Cache-Speicher ist offline',
  'storage.open': 'Speicher öffnen',

  'off.title': 'LanCache ist aus',
  'off.text':
    'PiCache kann Spiele-Downloads und Updates (Steam, Epic, Battle.net, Xbox, PlayStation und mehr) für dein ganzes Netzwerk cachen, sodass jeder Download nur einmal aus dem Internet kommt.',
  'off.step1': 'Prüfe den Speicher: Der Cache braucht eine Platte mit viel freiem Platz, am besten keine SD-Karte.',
  'off.step2':
    'Schalte LanCache in den Cache-Einstellungen ein. PiCache beantwortet dann die Download-Hosts mit seiner eigenen Adresse.',
  'off.step3': 'Starte einen Download auf einem Gerät, das PiCache als DNS-Server nutzt.',
  'off.enable': 'Cache-Einstellungen öffnen',
  'off.storage': 'Speicher prüfen',
}

export default de
