package domain

// The word lists a customer database's permanent name is drawn from (D-190).
//
// ⚠ CURATION IS A PRODUCT DECISION, NOT A DETAIL. Every pair here is a name a
// customer may live with forever, read out on a support call, and paste into a
// public runbook. The lists are therefore restricted to NEUTRAL adjectives and
// CONCRETE nouns (nature, terrain, plants, animals, everyday objects), and
// deliberately exclude:
//
//   - anything that could combine offensively with anything else in the other
//     list (the combination is what ships, never the word alone);
//   - anything resembling a company, product or trademark;
//   - anything evaluative ("best", "premium") — a name must not imply a tier;
//   - anything about people, bodies, politics, religion or nationality;
//   - homophones and near-spellings that are painful to dictate over a phone.
//
// The lists may only ever GROW. Removing a word does not un-mint the names
// already carrying it, and shrinking the space silently raises the collision
// rate; validateEndpointID checks the SHAPE of an id, never its membership here,
// precisely so an older name keeps validating forever.
//
// The space is |adjectives| × |nouns| × 10^4 ≈ 4×10^8, which is not a uniqueness
// guarantee and is not treated as one: the unique index on
// fleet_resources.endpoint_id is the only arbiter, and the mint path retries a
// bounded number of times on a collision.
var endpointAdjectives = []string{
	"amber", "ancient", "arctic", "autumn", "azure", "balmy", "brave", "breezy",
	"bright", "brisk", "bronze", "calm", "candid", "careful", "caring",
	"cheerful", "chilly", "civic", "classic", "clean", "clear", "clever",
	"cobalt", "cool", "copper", "cosmic", "cozy", "crimson", "crisp", "curly",
	"daily", "dainty", "dapper", "dewy", "dreamy", "dusky", "eager", "early",
	"earthy", "easy", "elder", "elegant", "emerald", "endless", "even",
	"fabled", "faithful", "famous", "fancy", "feathery", "fertile", "fine",
	"firm", "flowing", "fluent", "fluffy", "fond", "formal", "fragrant", "free",
	"fresh", "friendly", "frosty", "gentle", "giddy", "gilded", "glad",
	"gleaming", "gliding", "glowing", "golden", "graceful", "grand", "grassy",
	"green", "hearty", "helpful", "hidden", "honest", "hopeful", "humble",
	"icy", "indigo", "ivory", "jade", "jolly", "jovial", "joyful", "keen",
	"kind", "lasting", "leafy", "lively", "loyal", "lucid", "lucky", "lunar",
	"marble", "mellow", "merry", "mighty", "mild", "minty", "misty", "modest",
	"mossy", "muted", "narrow", "neat", "nimble", "noble", "northern", "olive",
	"opal", "open", "pale", "patient", "peaceful", "pearly", "placid", "plain",
	"pleasant", "plucky", "polar", "polite", "precise", "pretty", "prime",
	"proud", "quaint", "quick", "quiet", "rapid", "ready", "restful",
	"rippling", "robust", "rocky", "rosy", "royal", "ruby", "rustic", "sandy",
	"sapphire", "scenic", "serene", "shady", "sharp", "shiny", "silent",
	"silken", "silver", "simple", "sincere", "sleek", "slender", "smiling",
	"smooth", "snowy", "soaring", "soft", "solar", "solid", "sparkling",
	"spirited", "spry", "stable", "starry", "steady", "sterling", "still",
	"stony", "sturdy", "sunlit", "sunny", "swift", "tender", "thankful",
	"tidal", "tidy", "timely", "tranquil", "trusty", "twilight", "upbeat",
	"urban", "valiant", "velvet", "verdant", "vivid", "wandering", "warm",
	"wavy", "whimsical", "willing", "windy", "winsome", "wise", "woven",
	"zesty",
}

var endpointNouns = []string{
	"acorn", "alcove", "anchor", "anvil", "arbor", "arch", "ash", "aspen",
	"aurora", "badger", "basin", "basket", "bay", "beach", "beacon", "bell",
	"birch", "bison", "blossom", "bluff", "boulder", "bramble", "branch",
	"brook", "burrow", "cabin", "cactus", "canoe", "canopy", "canyon",
	"cascade", "castle", "cavern", "cedar", "chalk", "channel", "chapel",
	"clay", "cliff", "cloud", "clover", "coast", "cobble", "comet", "compass",
	"cottage", "cove", "crane", "crater", "creek", "crest", "crocus",
	"crystal", "cypress", "daisy", "dale", "dawn", "deer", "dew", "dove",
	"dune", "dusk", "egret", "elk", "ember", "falcon", "fawn", "fern", "field",
	"finch", "fjord", "flint", "forest", "fountain", "foxglove", "garden",
	"gate", "geyser", "glacier", "glade", "glen", "granite", "grotto", "grove",
	"gulf", "harbor", "hare", "harvest", "haven", "heath", "hedge", "heron",
	"hill", "horizon", "ibex", "iceberg", "inlet", "iris", "island", "ivy",
	"juniper", "kettle", "kite", "koi", "lagoon", "lake", "lantern", "lark",
	"ledge", "lily", "linden", "lodge", "lotus", "lynx", "maple", "marsh",
	"marten", "meadow", "mesa", "mill", "mist", "moor", "moss", "moth",
	"mountain", "nectar", "nest", "oak", "oasis", "orchard", "orchid", "otter",
	"owl", "palm", "pasture", "path", "pebble", "petal", "pier", "pine",
	"plateau", "pond", "poplar", "prairie", "puffin", "quail", "quarry",
	"quartz", "quill", "rapids", "raven", "ravine", "reef", "ridge", "rill",
	"river", "robin", "rowan", "sage", "sail", "sandbar", "sapling",
	"savanna", "seal", "sequoia", "shore", "shrub", "sierra", "silo", "sky",
	"slope", "sparrow", "spring", "spruce", "stream", "summit", "sunrise",
	"sunset", "swan", "thicket", "thistle", "tide", "timber", "trail", "tulip",
	"tundra", "valley", "vine", "violet", "vista", "wave", "willow", "window",
	"woodland", "wren", "yarrow", "zephyr",
}
