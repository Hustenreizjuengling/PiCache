package parental

import (
	"slices"
	"strings"
)

// Service is an app or website of the built-in catalogue. Domains are
// subtree matches: the domain and all its subdomains.
type Service struct {
	ID       string   `json:"id"`       // lower-case slug (frozen once released)
	Name     string   `json:"name"`     // display name (not translated)
	Category string   `json:"category"` // one of categories
	Domains  []string `json:"domains"`
}

// Categories in the order the catalogue is sorted by. privacy holds VPN and
// proxy apps, software app stores (never update channels), hosting file
// sharing and cloud storage.
var categories = []string{
	"video", "social", "messaging", "gaming", "music", "ai", "dating", "gambling", "shopping", "privacy",
	"software", "hosting", "news",
}

// catalogue is the built-in service list, sorted by category (in the order
// of categories) and then by name. Best effort, updated with releases and
// never downloaded at run time. Only well-known, service-specific domains
// belong here, never shared infrastructure (googleapis.com, akamaihd.net,
// cloudfront.net, fastly.net, …) nor anything whose blocking breaks
// unrelated things or security (OS and security updates, certificates,
// time, connectivity checks, identity providers; a vendor domain that also
// serves other products only with service-specific names below it, e.g.
// music.apple.com): blocking a service must not break others. Every domain
// was checked in DNS (a delegated zone) on 2026-09-26. Ids are frozen once
// released and domain sets only grow (TestFrozenServices); a domain leaves
// only when it is dead or turned out to be shared, noted in the CHANGELOG.
var catalogue = []Service{
	// video
	{ID: "crunchyroll", Name: "Crunchyroll", Category: "video", Domains: []string{
		"crunchyroll.com", "crunchyrollsvc.com", "vrv.co"}},
	{ID: "dailymotion", Name: "Dailymotion", Category: "video", Domains: []string{"dailymotion.com", "dmcdn.net"}},
	{ID: "dazn", Name: "DAZN", Category: "video", Domains: []string{"dazn.com", "daznservices.com", "indazn.com"}},
	{ID: "disneyplus", Name: "Disney+", Category: "video", Domains: []string{
		"disneyplus.com", "disney-plus.net", "dssott.com", "bamgrid.com", "disneystreaming.com"}},
	{ID: "hulu", Name: "Hulu", Category: "video", Domains: []string{"hulu.com", "hulustream.com", "huluim.com"}},
	{ID: "joyn", Name: "Joyn", Category: "video", Domains: []string{"joyn.de", "joyn.at", "joyn.ch"}},
	{ID: "kick", Name: "Kick", Category: "video", Domains: []string{"kick.com"}},
	{ID: "max", Name: "Max", Category: "video", Domains: []string{"max.com", "hbomax.com", "hbo.com", "hbomaxcdn.com"}},
	{ID: "netflix", Name: "Netflix", Category: "video", Domains: []string{
		"netflix.com", "netflix.net", "nflxext.com", "nflximg.com", "nflximg.net", "nflxso.net", "nflxvideo.net"}},
	{ID: "paramountplus", Name: "Paramount+", Category: "video", Domains: []string{
		"paramountplus.com", "pplusstatic.com", "cbsivideo.com"}},
	{ID: "primevideo", Name: "Prime Video", Category: "video", Domains: []string{
		"primevideo.com", "aiv-cdn.net", "aiv-delivery.net", "amazonvideo.com", "pv-cdn.net"}},
	{ID: "tiktok", Name: "TikTok", Category: "video", Domains: []string{
		"tiktok.com", "tiktokv.com", "tiktokv.us", "tiktokw.us", "tiktokcdn.com", "tiktokcdn-us.com", "tiktokcdn-eu.com",
		"byteoversea.com", "ibytedtos.com", "ibyteimg.com", "muscdn.com", "musical.ly", "tik-tokapi.com", "ttwstatic.com"}},
	{ID: "twitch", Name: "Twitch", Category: "video", Domains: []string{
		"twitch.tv", "ttvnw.net", "jtvnw.net", "twitchcdn.net", "twitchsvc.net", "ext-twitch.tv", "live-video.net"}},
	{ID: "vimeo", Name: "Vimeo", Category: "video", Domains: []string{"vimeo.com", "vimeocdn.com"}},
	{ID: "youtube", Name: "YouTube", Category: "video", Domains: []string{
		"youtube.com", "youtu.be", "yt.be", "ytimg.com", "googlevideo.com", "youtube-nocookie.com", "youtubekids.com",
		"youtubei.googleapis.com", "youtube.googleapis.com", "yt3.ggpht.com", "youtube-ui.l.google.com"}},
	// social
	{ID: "9gag", Name: "9GAG", Category: "social", Domains: []string{"9gag.com", "9cache.com"}},
	{ID: "bereal", Name: "BeReal", Category: "social", Domains: []string{"bereal.com", "bere.al"}},
	{ID: "bluesky", Name: "Bluesky", Category: "social", Domains: []string{"bsky.app", "bsky.social", "bsky.network"}},
	{ID: "facebook", Name: "Facebook", Category: "social", Domains: []string{
		"facebook.com", "facebook.net", "fb.com", "fb.me", "fbcdn.net", "fbsbx.com", "messenger.com", "m.me"}},
	{ID: "imgur", Name: "Imgur", Category: "social", Domains: []string{"imgur.com", "imgur.io"}},
	{ID: "instagram", Name: "Instagram", Category: "social", Domains: []string{
		"instagram.com", "cdninstagram.com", "ig.me", "instagr.am"}},
	{ID: "linkedin", Name: "LinkedIn", Category: "social", Domains: []string{"linkedin.com", "licdn.com", "lnkd.in"}},
	{ID: "mastodon", Name: "Mastodon", Category: "social", Domains: []string{
		"mastodon.social", "mastodon.online", "joinmastodon.org"}},
	{ID: "pinterest", Name: "Pinterest", Category: "social", Domains: []string{
		"pinterest.com", "pinimg.com", "pin.it", "pinterest.de", "pinterest.at", "pinterest.ch", "pinterest.co.uk", "pinterest.fr"}},
	{ID: "quora", Name: "Quora", Category: "social", Domains: []string{"quora.com", "quoracdn.net"}},
	{ID: "reddit", Name: "Reddit", Category: "social", Domains: []string{
		"reddit.com", "redd.it", "redditmedia.com", "redditstatic.com"}},
	{ID: "snapchat", Name: "Snapchat", Category: "social", Domains: []string{
		"snapchat.com", "snap.com", "sc-cdn.net", "sc-static.net", "sc-prod.net", "sc-gw.com", "snapads.com", "snapkit.co", "bitmoji.com"}},
	{ID: "threads", Name: "Threads", Category: "social", Domains: []string{"threads.net", "threads.com"}},
	{ID: "tumblr", Name: "Tumblr", Category: "social", Domains: []string{"tumblr.com"}},
	{ID: "vk", Name: "VK", Category: "social", Domains: []string{"vk.com", "vk.ru", "vkuser.net", "userapi.com"}},
	{ID: "x", Name: "X (Twitter)", Category: "social", Domains: []string{
		"x.com", "twitter.com", "twimg.com", "t.co", "twttr.com"}},
	// messaging
	{ID: "discord", Name: "Discord", Category: "messaging", Domains: []string{
		"discord.com", "discord.gg", "discordapp.com", "discordapp.net", "discord.media", "discordcdn.com"}},
	{ID: "line", Name: "LINE", Category: "messaging", Domains: []string{"line.me", "line-apps.com", "line-scdn.net"}},
	{ID: "teams", Name: "Microsoft Teams", Category: "messaging", Domains: []string{"teams.microsoft.com", "teams.live.com"}},
	{ID: "signal", Name: "Signal", Category: "messaging", Domains: []string{
		"signal.org", "whispersystems.org", "signal.art", "signal.me"}},
	{ID: "telegram", Name: "Telegram", Category: "messaging", Domains: []string{
		"telegram.org", "telegram.me", "t.me", "telesco.pe", "tdesktop.com", "telegra.ph"}},
	{ID: "threema", Name: "Threema", Category: "messaging", Domains: []string{"threema.ch"}},
	{ID: "viber", Name: "Viber", Category: "messaging", Domains: []string{"viber.com"}},
	{ID: "wechat", Name: "WeChat", Category: "messaging", Domains: []string{"wechat.com", "weixin.qq.com", "wx.qq.com"}},
	{ID: "whatsapp", Name: "WhatsApp", Category: "messaging", Domains: []string{"whatsapp.com", "whatsapp.net", "wa.me"}},
	{ID: "zoom", Name: "Zoom", Category: "messaging", Domains: []string{"zoom.us", "zoom.com"}},
	// gaming
	{ID: "activision", Name: "Activision (Call of Duty)", Category: "gaming", Domains: []string{"activision.com", "callofduty.com"}},
	{ID: "battlenet", Name: "Battle.net (Blizzard)", Category: "gaming", Domains: []string{"battle.net", "blizzard.com"}},
	{ID: "chesscom", Name: "Chess.com", Category: "gaming", Domains: []string{"chess.com", "chesscomfiles.com"}},
	{ID: "ea", Name: "EA (Electronic Arts)", Category: "gaming", Domains: []string{"ea.com", "origin.com"}},
	{ID: "fortnite", Name: "Fortnite / Epic Games", Category: "gaming", Domains: []string{
		"fortnite.com", "epicgames.com", "epicgames.dev"}},
	{ID: "gog", Name: "GOG", Category: "gaming", Domains: []string{"gog.com", "gog-statics.com"}},
	{ID: "hoyoverse", Name: "HoYoverse (Genshin Impact)", Category: "gaming", Domains: []string{
		"hoyoverse.com", "mihoyo.com", "hoyolab.com"}},
	{ID: "itchio", Name: "itch.io", Category: "gaming", Domains: []string{"itch.io", "itch.zone"}},
	{ID: "minecraft", Name: "Minecraft", Category: "gaming", Domains: []string{
		"minecraft.net", "mojang.com", "minecraftservices.com", "minecraft-services.net"}},
	{ID: "nintendo", Name: "Nintendo", Category: "gaming", Domains: []string{
		"nintendo.com", "nintendo.net", "nintendo.de", "nintendo-europe.com"}},
	{ID: "playstation", Name: "PlayStation Network", Category: "gaming", Domains: []string{
		"playstation.com", "playstation.net", "sonyentertainmentnetwork.com"}},
	{ID: "pokemongo", Name: "Pokémon GO", Category: "gaming", Domains: []string{"pokemongolive.com", "nianticlabs.com"}},
	{ID: "pubg", Name: "PUBG", Category: "gaming", Domains: []string{"pubg.com", "pubgmobile.com"}},
	{ID: "riotgames", Name: "Riot Games (League of Legends, Valorant)", Category: "gaming", Domains: []string{
		"riotgames.com", "leagueoflegends.com", "playvalorant.com", "riotcdn.net", "pvp.net"}},
	{ID: "roblox", Name: "Roblox", Category: "gaming", Domains: []string{
		"roblox.com", "rbxcdn.com", "rbx.com", "robloxlabs.com"}},
	{ID: "steam", Name: "Steam", Category: "gaming", Domains: []string{
		"steampowered.com", "steamcommunity.com", "steamstatic.com", "steamcontent.com", "steamserver.net", "steamgames.com", "steam-chat.com"}},
	{ID: "supercell", Name: "Supercell (Clash of Clans, Brawl Stars)", Category: "gaming", Domains: []string{
		"supercell.com", "clashofclans.com", "clashroyale.com", "brawlstars.com"}},
	{ID: "ubisoft", Name: "Ubisoft", Category: "gaming", Domains: []string{"ubisoft.com", "ubi.com", "ubisoftconnect.com"}},
	{ID: "xbox", Name: "Xbox network", Category: "gaming", Domains: []string{"xbox.com", "xboxlive.com", "xboxservices.com"}},
	// music
	{ID: "applemusic", Name: "Apple Music", Category: "music", Domains: []string{"music.apple.com"}},
	{ID: "audible", Name: "Audible", Category: "music", Domains: []string{"audible.com", "audible.de", "audible.co.uk"}},
	{ID: "bandcamp", Name: "Bandcamp", Category: "music", Domains: []string{"bandcamp.com", "bcbits.com"}},
	{ID: "deezer", Name: "Deezer", Category: "music", Domains: []string{"deezer.com", "dzcdn.net"}},
	{ID: "soundcloud", Name: "SoundCloud", Category: "music", Domains: []string{"soundcloud.com", "sndcdn.com"}},
	{ID: "spotify", Name: "Spotify", Category: "music", Domains: []string{
		"spotify.com", "scdn.co", "spotifycdn.com", "spotifycdn.net", "pscdn.co"}},
	{ID: "tidal", Name: "TIDAL", Category: "music", Domains: []string{"tidal.com", "tidalhifi.com"}},
	// ai
	{ID: "characterai", Name: "Character.AI", Category: "ai", Domains: []string{"character.ai", "c.ai"}},
	{ID: "chatgpt", Name: "ChatGPT", Category: "ai", Domains: []string{
		"chatgpt.com", "openai.com", "oaistatic.com", "oaiusercontent.com"}},
	{ID: "claude", Name: "Claude", Category: "ai", Domains: []string{"claude.ai", "anthropic.com"}},
	{ID: "deepseek", Name: "DeepSeek", Category: "ai", Domains: []string{"deepseek.com"}},
	{ID: "gemini", Name: "Gemini", Category: "ai", Domains: []string{"gemini.google.com"}},
	{ID: "grok", Name: "Grok", Category: "ai", Domains: []string{"grok.com", "x.ai"}},
	{ID: "mistral", Name: "Le Chat (Mistral AI)", Category: "ai", Domains: []string{"mistral.ai"}},
	{ID: "metaai", Name: "Meta AI", Category: "ai", Domains: []string{"meta.ai"}},
	{ID: "copilot", Name: "Microsoft Copilot", Category: "ai", Domains: []string{"copilot.microsoft.com", "copilot.cloud.microsoft"}},
	{ID: "perplexity", Name: "Perplexity", Category: "ai", Domains: []string{"perplexity.ai", "pplx.ai"}},
	{ID: "replika", Name: "Replika", Category: "ai", Domains: []string{"replika.com", "replika.ai"}},
	// dating
	{ID: "badoo", Name: "Badoo", Category: "dating", Domains: []string{"badoo.com", "badoocdn.com"}},
	{ID: "bumble", Name: "Bumble", Category: "dating", Domains: []string{"bumble.com"}},
	{ID: "grindr", Name: "Grindr", Category: "dating", Domains: []string{"grindr.com", "grindr.mobi"}},
	{ID: "hinge", Name: "Hinge", Category: "dating", Domains: []string{"hinge.co"}},
	{ID: "lovoo", Name: "LOVOO", Category: "dating", Domains: []string{"lovoo.com", "lovoo.net"}},
	{ID: "match", Name: "Match", Category: "dating", Domains: []string{"match.com"}},
	{ID: "okcupid", Name: "OkCupid", Category: "dating", Domains: []string{"okcupid.com"}},
	{ID: "parship", Name: "Parship", Category: "dating", Domains: []string{"parship.de", "parship.com"}},
	{ID: "pof", Name: "Plenty of Fish", Category: "dating", Domains: []string{"pof.com"}},
	{ID: "tinder", Name: "Tinder", Category: "dating", Domains: []string{"tinder.com", "gotinder.com"}},
	// gambling
	{ID: "bet365", Name: "bet365", Category: "gambling", Domains: []string{"bet365.com", "bet365.de"}},
	{ID: "betfair", Name: "Betfair", Category: "gambling", Domains: []string{"betfair.com"}},
	{ID: "betway", Name: "Betway", Category: "gambling", Domains: []string{"betway.com", "betway.de"}},
	{ID: "bwin", Name: "bwin", Category: "gambling", Domains: []string{"bwin.com", "bwin.de"}},
	{ID: "lottoland", Name: "Lottoland", Category: "gambling", Domains: []string{"lottoland.com"}},
	{ID: "pokerstars", Name: "PokerStars", Category: "gambling", Domains: []string{"pokerstars.com", "pokerstars.eu", "pokerstars.de"}},
	{ID: "stake", Name: "Stake", Category: "gambling", Domains: []string{"stake.com", "stake.us"}},
	{ID: "tipico", Name: "Tipico", Category: "gambling", Domains: []string{"tipico.de", "tipico.com"}},
	{ID: "unibet", Name: "Unibet", Category: "gambling", Domains: []string{"unibet.com"}},
	// shopping
	{ID: "aliexpress", Name: "AliExpress", Category: "shopping", Domains: []string{
		"aliexpress.com", "aliexpress.us", "aliexpress-media.com"}},
	{ID: "amazonshop", Name: "Amazon (shop)", Category: "shopping", Domains: []string{
		"www.amazon.com", "www.amazon.de", "www.amazon.co.uk", "www.amazon.fr", "www.amazon.it", "www.amazon.es", "www.amazon.nl"}},
	{ID: "ebay", Name: "eBay", Category: "shopping", Domains: []string{
		"ebay.com", "ebay.de", "ebay.co.uk", "ebayimg.com", "ebaystatic.com"}},
	{ID: "etsy", Name: "Etsy", Category: "shopping", Domains: []string{"etsy.com", "etsystatic.com"}},
	{ID: "kleinanzeigen", Name: "Kleinanzeigen", Category: "shopping", Domains: []string{"kleinanzeigen.de"}},
	{ID: "otto", Name: "OTTO", Category: "shopping", Domains: []string{"otto.de"}},
	{ID: "shein", Name: "SHEIN", Category: "shopping", Domains: []string{"shein.com", "ltwebstatic.com"}},
	{ID: "temu", Name: "Temu", Category: "shopping", Domains: []string{"temu.com"}},
	{ID: "vinted", Name: "Vinted", Category: "shopping", Domains: []string{"vinted.com", "vinted.de", "vinted.net"}},
	{ID: "zalando", Name: "Zalando", Category: "shopping", Domains: []string{"zalando.de", "zalando.com", "ztat.net"}},
	// privacy (VPN and proxy apps)
	{ID: "cyberghost", Name: "CyberGhost", Category: "privacy", Domains: []string{"cyberghostvpn.com"}},
	{ID: "expressvpn", Name: "ExpressVPN", Category: "privacy", Domains: []string{"expressvpn.com"}},
	{ID: "hotspotshield", Name: "Hotspot Shield", Category: "privacy", Domains: []string{"hotspotshield.com"}},
	{ID: "mullvad", Name: "Mullvad", Category: "privacy", Domains: []string{"mullvad.net"}},
	{ID: "nordvpn", Name: "NordVPN", Category: "privacy", Domains: []string{"nordvpn.com", "nordvpn.net", "nordcdn.com"}},
	{ID: "pia", Name: "Private Internet Access", Category: "privacy", Domains: []string{"privateinternetaccess.com"}},
	{ID: "protonvpn", Name: "Proton VPN", Category: "privacy", Domains: []string{"protonvpn.com", "protonvpn.ch"}},
	{ID: "psiphon", Name: "Psiphon", Category: "privacy", Domains: []string{"psiphon.ca", "psiphon3.com"}},
	{ID: "surfshark", Name: "Surfshark", Category: "privacy", Domains: []string{"surfshark.com"}},
	{ID: "tor", Name: "Tor Project", Category: "privacy", Domains: []string{"torproject.org"}},
	{ID: "tunnelbear", Name: "TunnelBear", Category: "privacy", Domains: []string{"tunnelbear.com"}},
	{ID: "windscribe", Name: "Windscribe", Category: "privacy", Domains: []string{"windscribe.com", "windscribe.net"}},
	// software (app stores)
	{ID: "apkpure", Name: "APKPure", Category: "software", Domains: []string{"apkpure.com", "apkpure.net"}},
	{ID: "appstore", Name: "App Store (Apple)", Category: "software", Domains: []string{"apps.apple.com"}},
	{ID: "aptoide", Name: "Aptoide", Category: "software", Domains: []string{"aptoide.com"}},
	{ID: "fdroid", Name: "F-Droid", Category: "software", Domains: []string{"f-droid.org"}},
	{ID: "galaxystore", Name: "Galaxy Store (Samsung)", Category: "software", Domains: []string{"galaxystore.samsung.com"}},
	{ID: "googleplay", Name: "Google Play", Category: "software", Domains: []string{"play.google.com"}},
	{ID: "appgallery", Name: "Huawei AppGallery", Category: "software", Domains: []string{"appgallery.huawei.com"}},
	{ID: "microsoftstore", Name: "Microsoft Store", Category: "software", Domains: []string{"apps.microsoft.com"}},
	{ID: "uptodown", Name: "Uptodown", Category: "software", Domains: []string{"uptodown.com"}},
	// hosting (file sharing, cloud storage)
	{ID: "box", Name: "Box", Category: "hosting", Domains: []string{"box.com", "box.net", "boxcdn.net"}},
	{ID: "dropbox", Name: "Dropbox", Category: "hosting", Domains: []string{
		"dropbox.com", "dropboxapi.com", "dropboxusercontent.com", "db.tt"}},
	{ID: "googledrive", Name: "Google Drive", Category: "hosting", Domains: []string{"drive.google.com"}},
	{ID: "mediafire", Name: "MediaFire", Category: "hosting", Domains: []string{"mediafire.com"}},
	{ID: "mega", Name: "MEGA", Category: "hosting", Domains: []string{"mega.nz", "mega.io", "mega.co.nz"}},
	{ID: "onedrive", Name: "OneDrive", Category: "hosting", Domains: []string{
		"onedrive.com", "onedrive.live.com", "1drv.com", "1drv.ms"}},
	{ID: "pcloud", Name: "pCloud", Category: "hosting", Domains: []string{"pcloud.com", "pcloud.link"}},
	{ID: "wetransfer", Name: "WeTransfer", Category: "hosting", Domains: []string{"wetransfer.com", "we.tl", "wetransfer.net"}},
	// news
	{ID: "bbc", Name: "BBC", Category: "news", Domains: []string{"bbc.com", "bbc.co.uk", "bbci.co.uk"}},
	{ID: "bild", Name: "BILD", Category: "news", Domains: []string{"bild.de"}},
	{ID: "cnn", Name: "CNN", Category: "news", Domains: []string{"cnn.com"}},
	{ID: "spiegel", Name: "DER SPIEGEL", Category: "news", Domains: []string{"spiegel.de"}},
	{ID: "googlenews", Name: "Google News", Category: "news", Domains: []string{"news.google.com"}},
	{ID: "tagesschau", Name: "tagesschau", Category: "news", Domains: []string{"tagesschau.de"}},
	{ID: "guardian", Name: "The Guardian", Category: "news", Domains: []string{"theguardian.com", "guim.co.uk"}},
	{ID: "nytimes", Name: "The New York Times", Category: "news", Domains: []string{"nytimes.com", "nyt.com"}},
}

// serviceIndex maps a service id to its position in catalogue, and
// serviceDomains every catalogue domain to its service's position (the
// service matcher of the hot path).
var (
	serviceIndex   = map[string]int{}
	serviceDomains = map[string]int{}
)

func init() {
	for i, s := range catalogue {
		serviceIndex[s.ID] = i
		for _, d := range s.Domains {
			serviceDomains[d] = i
		}
	}
}

// Services returns the service catalogue (GET /parental/services).
func Services() []Service {
	out := make([]Service, len(catalogue))
	for i, s := range catalogue {
		s.Domains = slices.Clone(s.Domains)
		out[i] = s
	}
	return out
}

// maxLabels bounds the suffix walk (a DNS name has at most 127 labels).
const maxLabels = 127

// serviceOf returns the catalogue position of the service whose domain is
// qname or one of its parent domains (-1 if none). qname is lower-case
// without a trailing dot. It does not allocate.
func serviceOf(qname string) int {
	for i, n := 0, 0; n < maxLabels && i < len(qname); n++ {
		if idx, ok := serviceDomains[qname[i:]]; ok {
			return idx
		}
		j := strings.IndexByte(qname[i:], '.')
		if j < 0 {
			break
		}
		i += j + 1
	}
	return -1
}
