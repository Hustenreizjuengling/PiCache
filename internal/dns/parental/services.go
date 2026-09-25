package parental

import (
	"slices"
	"strings"
)

// Service is an app or website of the built-in catalogue. Domains are
// subtree matches: the domain and all its subdomains.
type Service struct {
	ID       string   `json:"id"`       // lower-case slug
	Name     string   `json:"name"`     // display name (not translated)
	Category string   `json:"category"` // video | social | messaging | gaming | music | ai
	Domains  []string `json:"domains"`
}

// Categories in the order the catalogue is sorted by.
var categories = []string{"video", "social", "messaging", "gaming", "music", "ai"}

// catalogue is the built-in service list, sorted by category (in the order
// of categories) and then by name. Best effort, updated with releases and
// never downloaded at run time. Only well-known, service-specific domains
// belong here, never shared infrastructure (googleapis.com, akamaihd.net,
// cloudfront.net, fastly.net, …): blocking a service must not break others.
var catalogue = []Service{
	// video
	{ID: "disneyplus", Name: "Disney+", Category: "video", Domains: []string{
		"disneyplus.com", "disney-plus.net", "dssott.com", "bamgrid.com", "disneystreaming.com"}},
	{ID: "kick", Name: "Kick", Category: "video", Domains: []string{"kick.com"}},
	{ID: "netflix", Name: "Netflix", Category: "video", Domains: []string{
		"netflix.com", "netflix.net", "nflxext.com", "nflximg.com", "nflximg.net", "nflxso.net", "nflxvideo.net"}},
	{ID: "primevideo", Name: "Prime Video", Category: "video", Domains: []string{
		"primevideo.com", "aiv-cdn.net", "aiv-delivery.net", "amazonvideo.com", "pv-cdn.net"}},
	{ID: "tiktok", Name: "TikTok", Category: "video", Domains: []string{
		"tiktok.com", "tiktokv.com", "tiktokv.us", "tiktokw.us", "tiktokcdn.com", "tiktokcdn-us.com", "tiktokcdn-eu.com",
		"byteoversea.com", "ibytedtos.com", "ibyteimg.com", "muscdn.com", "musical.ly", "tik-tokapi.com", "ttwstatic.com"}},
	{ID: "twitch", Name: "Twitch", Category: "video", Domains: []string{
		"twitch.tv", "ttvnw.net", "jtvnw.net", "twitchcdn.net", "twitchsvc.net", "ext-twitch.tv", "live-video.net"}},
	{ID: "youtube", Name: "YouTube", Category: "video", Domains: []string{
		"youtube.com", "youtu.be", "yt.be", "ytimg.com", "googlevideo.com", "youtube-nocookie.com", "youtubekids.com",
		"youtubei.googleapis.com", "youtube.googleapis.com", "yt3.ggpht.com", "youtube-ui.l.google.com"}},
	// social
	{ID: "bereal", Name: "BeReal", Category: "social", Domains: []string{"bereal.com", "bere.al"}},
	{ID: "facebook", Name: "Facebook", Category: "social", Domains: []string{
		"facebook.com", "facebook.net", "fb.com", "fb.me", "fbcdn.net", "fbsbx.com", "messenger.com", "m.me"}},
	{ID: "instagram", Name: "Instagram", Category: "social", Domains: []string{
		"instagram.com", "cdninstagram.com", "ig.me", "instagr.am"}},
	{ID: "pinterest", Name: "Pinterest", Category: "social", Domains: []string{
		"pinterest.com", "pinimg.com", "pin.it", "pinterest.de", "pinterest.at", "pinterest.ch", "pinterest.co.uk", "pinterest.fr"}},
	{ID: "reddit", Name: "Reddit", Category: "social", Domains: []string{
		"reddit.com", "redd.it", "redditmedia.com", "redditstatic.com"}},
	{ID: "snapchat", Name: "Snapchat", Category: "social", Domains: []string{
		"snapchat.com", "snap.com", "sc-cdn.net", "sc-static.net", "sc-prod.net", "sc-gw.com", "snapads.com", "snapkit.co", "bitmoji.com"}},
	{ID: "threads", Name: "Threads", Category: "social", Domains: []string{"threads.net", "threads.com"}},
	{ID: "x", Name: "X (Twitter)", Category: "social", Domains: []string{
		"x.com", "twitter.com", "twimg.com", "t.co", "twttr.com"}},
	// messaging
	{ID: "discord", Name: "Discord", Category: "messaging", Domains: []string{
		"discord.com", "discord.gg", "discordapp.com", "discordapp.net", "discord.media", "discordcdn.com"}},
	{ID: "telegram", Name: "Telegram", Category: "messaging", Domains: []string{
		"telegram.org", "telegram.me", "t.me", "telesco.pe", "tdesktop.com", "telegra.ph"}},
	{ID: "whatsapp", Name: "WhatsApp", Category: "messaging", Domains: []string{"whatsapp.com", "whatsapp.net", "wa.me"}},
	// gaming
	{ID: "fortnite", Name: "Fortnite / Epic Games", Category: "gaming", Domains: []string{
		"fortnite.com", "epicgames.com", "epicgames.dev"}},
	{ID: "minecraft", Name: "Minecraft", Category: "gaming", Domains: []string{
		"minecraft.net", "mojang.com", "minecraftservices.com", "minecraft-services.net"}},
	{ID: "nintendo", Name: "Nintendo", Category: "gaming", Domains: []string{
		"nintendo.com", "nintendo.net", "nintendo.de", "nintendo-europe.com"}},
	{ID: "playstation", Name: "PlayStation Network", Category: "gaming", Domains: []string{
		"playstation.com", "playstation.net", "sonyentertainmentnetwork.com"}},
	{ID: "roblox", Name: "Roblox", Category: "gaming", Domains: []string{
		"roblox.com", "rbxcdn.com", "rbx.com", "robloxlabs.com"}},
	{ID: "steam", Name: "Steam", Category: "gaming", Domains: []string{
		"steampowered.com", "steamcommunity.com", "steamstatic.com", "steamcontent.com", "steamserver.net", "steamgames.com", "steam-chat.com"}},
	{ID: "xbox", Name: "Xbox network", Category: "gaming", Domains: []string{"xbox.com", "xboxlive.com", "xboxservices.com"}},
	// music
	{ID: "spotify", Name: "Spotify", Category: "music", Domains: []string{
		"spotify.com", "scdn.co", "spotifycdn.com", "spotifycdn.net", "pscdn.co"}},
	// ai
	{ID: "chatgpt", Name: "ChatGPT", Category: "ai", Domains: []string{
		"chatgpt.com", "openai.com", "oaistatic.com", "oaiusercontent.com"}},
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
