package battlenet

// Product is a game the prefill offers.
type Product struct {
	Code, Name string
}

// Products are Blizzard's, Activision's and Microsoft's games on
// Battle.net whose builds can be prefilled without keys (not Sea of
// Thieves and The Witcher 3: their build configs are encrypted).
var Products = []Product{
	{"wow", "World of Warcraft"},
	{"wow_classic", "World of Warcraft Classic"},
	{"wow_classic_era", "World of Warcraft Classic Era"},
	{"wow_anniversary", "World of Warcraft Classic Anniversary"},
	{"pro", "Overwatch 2"},
	{"fenris", "Diablo IV"},
	{"osi", "Diablo II: Resurrected"},
	{"d3", "Diablo III"},
	{"anbs", "Diablo Immortal"},
	{"drtl", "Diablo"},
	{"hsb", "Hearthstone"},
	{"hero", "Heroes of the Storm"},
	{"s2", "StarCraft II"},
	{"s1", "StarCraft: Remastered"},
	{"w3", "Warcraft III: Reforged"},
	{"w2r", "Warcraft II: Remastered"},
	{"w1r", "Warcraft I: Remastered"},
	{"w2bn", "Warcraft II: Battle.net Edition"},
	{"war1", "Warcraft: Orcs & Humans"},
	{"gryphon", "Warcraft Rumble"},
	{"rtro", "Blizzard Arcade Collection"},
	{"auks", "Call of Duty"},
	{"btlr", "Call of Duty: Black Ops 6"},
	{"pinta", "Call of Duty: Modern Warfare III"},
	{"nina", "Call of Duty: Modern Warfare II"},
	{"fore", "Call of Duty: Vanguard"},
	{"zeus", "Call of Duty: Black Ops Cold War"},
	{"odin", "Call of Duty: Modern Warfare (2019)"},
	{"viper", "Call of Duty: Black Ops 4"},
	{"lazr", "Call of Duty: Modern Warfare 2 Campaign Remastered"},
	{"wlby", "Crash Bandicoot 4: It's About Time"},
	{"lbra", "Tony Hawk's Pro Skater 3 + 4"},
	{"aqua", "Avowed"},
	{"aris", "DOOM: The Dark Ages"},
	{"ark", "The Outer Worlds 2"},
}

// ProductName names a product code.
func ProductName(code string) string {
	for _, p := range Products {
		if p.Code == code {
			return p.Name
		}
	}
	return code
}
