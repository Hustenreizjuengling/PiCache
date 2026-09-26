// Anmeldung und Ersteinrichtung.

import type en from '../en/auth'
import type { Messages } from '../types'

const de: Messages<typeof en> = {
  'login.title': 'Bei PiCache anmelden',
  'login.username': 'Benutzername',
  'login.password': 'Passwort',
  'login.submit': 'Anmelden',
  'login.wrong': 'Benutzername oder Passwort ist falsch.',
  'login.throttled':
    'Zu viele Versuche. Warte einen Moment und versuche es erneut. Nach mehreren Fehlversuchen von diesem Gerät ist die Anmeldung hier 15 Minuten gesperrt. Passiert das immer wieder, melde dich mit einem Browser an, mit dem du PiCache schon benutzt hast.',
  'login.expired': 'Deine Sitzung ist abgelaufen. Melde dich erneut an, um dort weiterzumachen, wo du warst.',
  'login.totpTitle': 'Code eingeben',
  'login.totpHelp': 'Öffne deine Authenticator-App und gib den 6-stelligen Code für PiCache ein.',
  'login.code': 'Bestätigungscode',
  'login.codeFormat': 'Der Code hat 6 Ziffern.',
  'login.codeWrong': 'Der Code ist falsch oder abgelaufen. Gib den aktuellen Code aus deiner App ein.',
  'login.verify': 'Bestätigen und anmelden',
  'login.back': 'Anderes Konto verwenden',

  'setup.title': 'PiCache einrichten',
  'setup.intro':
    'Lege das Administratorkonto an. Du brauchst dafür das einmalige Einrichtungs-Token, das PiCache beim ersten Start erzeugt hat.',
  'setup.token': 'Einrichtungs-Token',
  'setup.tokenRequired': 'Gib das Einrichtungs-Token ein.',
  'setup.hintsTitle': 'Hier findest du das Einrichtungs-Token:',
  'setup.hintsDefault': 'Im PiCache-Log, oder führe „picache setup-token“ auf dem PiCache-Rechner aus.',
  'setup.username': 'Benutzername',
  'setup.usernameRequired': 'Wähle einen Benutzernamen.',
  'setup.password': 'Passwort',
  'setup.passwordHelp': 'Mindestens 10 Zeichen. Eine Passphrase aus vier oder mehr zufälligen Wörtern eignet sich gut.',
  'setup.passwordShort': 'Verwende mindestens {min} Zeichen.',
  'setup.repeat': 'Passwort wiederholen',
  'setup.mismatch': 'Die Passwörter stimmen nicht überein.',
  'setup.strength.tooShort': 'Zu kurz: mindestens {min} Zeichen',
  'setup.strength.weak': 'Schwach: leicht zu erraten',
  'setup.strength.fair': 'Mittel: länger ist besser',
  'setup.strength.good': 'Gut',
  'setup.strength.strong': 'Stark',
  'setup.forbidden': 'Das Einrichtungs-Token ist falsch oder PiCache ist bereits eingerichtet. Prüfe das Token oder melde dich an.',
  'setup.submit': 'Konto anlegen',

  'https.title': 'Diese Verbindung ist nicht verschlüsselt',
  'https.text':
    'Hier eingegebene Passwörter und das Einrichtungs-Token gehen unverschlüsselt über das Netzwerk. Verwende stattdessen die HTTPS-Adresse:',
  'https.cert':
    'Solange dieses Gerät dem Zertifikat von PiCache nicht vertraut (etwa über seine lokale CA, System > HTTPS-Zertifikat), fragt dein Browser einmal, ob du es akzeptierst.',
}

export default de
