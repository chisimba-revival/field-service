-- 0014_species_expansion.sql — comprehensive South African species catalogue.
--
-- Expands the catalogue from the original 7 entries to a comprehensive list
-- of South African mammals, birds and reptiles that a guide or trainee
-- would encounter on a reserve.
--
-- Codes are 4 uppercase letters, per the species_code_well_formed constraint.
-- Existing entries (LION, LEOP, ELEP, WHRI, BLRI, BUFA) are preserved.
-- Re-running this migration will not overwrite existing entries.

-- ---------------------------------------------------------------------------
-- Large mammals (Big 5 already exist: LION, LEOP, ELEP, WHRI, BLRI, BUFA)
-- ---------------------------------------------------------------------------

insert into species (code, common_name, scientific_name, description) values
  ('GIRA', 'Giraffe', 'Giraffa camelopardalis',
   'The tallest land animal, with a distinctive spotted pattern and long neck. Bulls can reach 5.5 metres.'),
  ('HIPP', 'Hippopotamus', 'Hippopotamus amphibius',
   'Semi-aquatic herbivore that spends days in water and grazes on land at night. Highly territorial and dangerous.'),
  ('ZEBR', 'Plains Zebra', 'Equus quagga',
   'Striped equid with distinctive black-and-white patterns unique to each individual. Lives in herds.'),
  ('WILD', 'Blue Wildebeest', 'Connochaetes taurinus',
   'Large antelope with a muscular front and slender rear. Known for long migrations and dramatic river crossings.'),
  ('CHEE', 'Cheetah', 'Acinonyx jubatus',
   'The fastest land animal, capable of reaching 112 km/h in short bursts. Distinguished from leopards by solid black spots and tear marks.'),
  ('WDOG', 'African Wild Dog', 'Lycaon pictus',
   'Endangered pack hunter with a mottled coat of black, white and tan. One of Africa''s most efficient predators.'),
  ('HYEN', 'Spotted Hyena', 'Crocuta crocuta',
   'Highly social predator and scavenger with a complex clan structure. Females are larger and dominant.'),
  ('BJAC', 'Black-backed Jackal', 'Canis mesomelas',
   'Opportunistic omnivore with a distinctive black and silver back. Often seen at dusk and dawn.'),
  ('SJAC', 'Side-striped Jackal', 'Canis adustus',
   'Smaller than the black-backed jackal, with a white-tipped tail and side stripe. Prefers thicker bush.'),
  ('CARA', 'Caracal', 'Caracara caracal',
   'Medium-sized cat with distinctive black tufted ears. An agile hunter capable of catching birds in flight.'),
  ('SERV', 'Serval', 'Leptailurus serval',
   'Slender, long-legged cat with large ears and a golden coat with black spots. Specialises in catching rodents.')
on conflict (code) do nothing;

-- ---------------------------------------------------------------------------
-- Antelope
-- ---------------------------------------------------------------------------

insert into species (code, common_name, scientific_name, description) values
  ('ELAN', 'Common Eland', 'Taurotragus oryx',
   'The largest antelope in Africa, with a tan coat and vertical white stripes. Both sexes have spiralled horns.'),
  ('KUDU', 'Greater Kudu', 'Tragelaphus strepsiceros',
   'Large antelope with spectacular spiralled horns in males. Grey-brown coat with vertical white stripes.'),
  ('NYAL', 'Nyala', 'Tragelaphus angasii',
   'Medium-sized antelope with a shaggy coat. Males are dark brown with white stripes; females are reddish-brown.'),
  ('BUSH', 'Bushbuck', 'Tragelaphus scriptus',
   'Solitary antelope with a reddish-brown coat and white spots and stripes. Prefers dense bush near water.'),
  ('IMPA', 'Impala', 'Aepyceros melampus',
   'Medium-sized antelope with a reddish-brown coat and white underparts. Known for spectacular leaping ability.'),
  ('SPRI', 'Springbok', 'Antidorcas marsupialis',
   'Small, graceful antelope with a white face and reddish-brown coat. Famous for pronking (high springing leaps).'),
  ('BLES', 'Blesbok', 'Damaliscus pygargus phillipsi',
   'Medium-sized antelope with a distinctive white face and blaze. Reddish-brown coat with a glossy sheen.'),
  ('BONT', 'Bontebok', 'Damaliscus pygargus pygargus',
   'Dark brown antelope with a distinctive white face blaze and white underparts. Once nearly extinct.'),
  ('ROAN', 'Roan Antelope', 'Hippotragus equinus',
   'Large antelope with a reddish-brown coat and a distinctive black and white face. Both sexes have backward-curving horns.'),
  ('SABL', 'Sable Antelope', 'Hippotragus niger',
   'Large, majestic antelope with a black coat in males and chestnut in females. Both sexes have long, scimitar-shaped horns.'),
  ('WATE', 'Waterbuck', 'Kobus ellipsiprymnus',
   'Large, shaggy antelope with a distinctive white ring on the rump. Always found near water.'),
  ('REEB', 'Common Reedbuck', 'Redunca arundinum',
   'Medium-sized antelope with a greyish-brown coat and a distinctive white underparts. Prefers reedbeds and tall grass.'),
  ('MREE', 'Mountain Reedbuck', 'Redunca fulvorufula',
   'Smaller than the common reedbuck, with a grey coat and a distinctive white patch under the tail. Found in rocky hills.'),
  ('GREY', 'Grey Rhebok', 'Pelea capreolus',
   'Small, grey antelope with a distinctive white face and underparts. Found in mountainous grassland.'),
  ('KLIP', 'Klipspringer', 'Oreotragus oreotragus',
   'Tiny, stocky antelope that stands on the tips of its hooves. Found on rocky outcrops and cliffs.'),
  ('STEE', 'Steenbok', 'Raphicerus campestris',
   'Small, delicate antelope with a reddish-brown coat and large ears. Often seen alone or in pairs.'),
  ('GRYS', 'Cape Grysbok', 'Raphicerus melanotis',
   'Small, shy antelope with a reddish-brown coat and white speckles. Prefers dense fynbos and renosterveld.'),
  ('ORIB', 'Oribi', 'Ourebia ourebi',
   'Small, slender antelope with a reddish-brown coat and a distinctive black scent gland under the ear.'),
  ('DUIK', 'Common Duiker', 'Sylvicapra grimmia',
   'Small, shy antelope with a greyish-brown coat. The name means "diver" — it dashes into cover when disturbed.'),
  ('BDUI', 'Blue Duiker', 'Philantomba monticola',
   'Tiny forest antelope with a bluish-grey coat. One of the smallest antelopes in Africa.'),
  ('REDD', 'Red Duiker', 'Cephalophus natalensis',
   'Small, reddish-brown forest antelope. Secretive and rarely seen in dense bush.')
on conflict (code) do nothing;

-- ---------------------------------------------------------------------------
-- Small mammals
-- ---------------------------------------------------------------------------

insert into species (code, common_name, scientific_name, description) values
  ('AARD', 'Aardvark', 'Orycteropus afer',
   'Nocturnal, burrowing mammal with a long snout and sticky tongue. Feeds almost exclusively on termites and ants.'),
  ('PANG', 'Ground Pangolin', 'Smutsia temminckii',
   'Scaly, nocturnal mammal that rolls into a ball when threatened. Feeds on ants and termites.'),
  ('HONE', 'Honey Badger', 'Mellivora capensis',
   'Fearless, stocky mustelid known for its toughness and intelligence. Will attack animals much larger than itself.'),
  ('CLAW', 'African Clawless Otter', 'Aonyx capensis',
   'Semi-aquatic mustelid with webbed feet and no claws on the front feet. Found near rivers, lakes and dams.'),
  ('SPOT', 'Spotted-necked Otter', 'Hydrictis maculicollis',
   'Semi-aquatic mustelid with distinctive white spots on the neck and throat. An excellent swimmer.'),
  ('BAND', 'Banded Mongoose', 'Mungos mungo',
   'Small, social mongoose with distinctive dark bands across the back. Lives in large packs.'),
  ('DWAR', 'Dwarf Mongoose', 'Helogale parvula',
   'The smallest mongoose in Africa, with a reddish-brown coat. Lives in family groups and uses termite mounds as dens.'),
  ('LARG', 'Large Grey Mongoose', 'Herpestes ichneumon',
   'The largest mongoose in Africa, with a greyish-brown coat. Often seen near water.'),
  ('SLEN', 'Slender Mongoose', 'Galerella sanguinea',
   'Small, slender mongoose with a reddish-brown coat. Common in a wide range of habitats.'),
  ('WMON', 'Water Mongoose', 'Atilax paludinosus',
   'Semi-aquatic mongoose with a dark brown coat. Found near water and an excellent swimmer.'),
  ('WHIT', 'White-tailed Mongoose', 'Ichneumia albicauda',
   'Large mongoose with a distinctive white-tipped tail. Nocturnal and solitary.'),
  ('MEER', 'Meerkat', 'Suricata suricatta',
   'Small, social mongoose that lives in large family groups. Famous for standing upright on sentry duty.'),
  ('GENA', 'Common Genet', 'Genetta genetta',
   'Small, cat-like carnivore with a spotted coat and a long, ringed tail. Nocturnal and arboreal.'),
  ('CIVE', 'African Civet', 'Civettictis civetta',
   'Large, cat-like carnivore with a spotted and striped coat. Nocturnal and omnivorous.'),
  ('PORC', 'Cape Porcupine', 'Hystrix africaeaustralis',
   'The largest rodent in Africa, with long, sharp quills. Nocturnal and herbivorous.'),
  ('HARE', 'Cape Hare', 'Lepus capensis',
   'Large hare with long ears and powerful hind legs. Nocturnal and solitary.'),
  ('SPRH', 'Springhare', 'Pedetes capensis',
   'Nocturnal, kangaroo-like rodent with long hind legs and a long tail. Lives in burrows.')
on conflict (code) do nothing;

-- ---------------------------------------------------------------------------
-- Primates
-- ---------------------------------------------------------------------------

insert into species (code, common_name, scientific_name, description) values
  ('VERV', 'Vervet Monkey', 'Chlorocebus pygerythrus',
   'Medium-sized monkey with a grey coat and black face. Highly adaptable and often seen near human habitation.'),
  ('BABO', 'Chacma Baboon', 'Papio ursinus',
   'Large, terrestrial monkey with a distinctive dog-like muzzle. Lives in large troops with a strict hierarchy.'),
  ('GALA', 'Greater Galago', 'Otolemur crassicaudatus',
   'Nocturnal primate with large eyes and ears. Also known as a bushbaby. Moves by leaping between branches.'),
  ('SAMA', 'Samango Monkey', 'Cercopithecus mitis',
   'Arboreal monkey with a dark coat and a white throat patch. Found in forest and riverine bush.')
on conflict (code) do nothing;

-- ---------------------------------------------------------------------------
-- Birds
-- ---------------------------------------------------------------------------

insert into species (code, common_name, scientific_name, description) values
  ('OSTR', 'Common Ostrich', 'Struthio camelus',
   'The largest bird in the world, flightless with long legs and a long neck. Males are black with white wing and tail plumes.'),
  ('SECB', 'Secretarybird', 'Sagittarius serpentarius',
   'Large, terrestrial raptor with long legs and a distinctive crest of black feathers. Hunts on foot, stamping on prey.'),
  ('KORI', 'Kori Bustard', 'Ardeotis kori',
   'The heaviest flying bird in Africa, with a large body and long legs. Males have a distinctive courtship display.'),
  ('AFSE', 'African Fish Eagle', 'Haliaeetus vocifer',
   'Large raptor with a distinctive white head and chest. Famous for its loud, carrying call — the "voice of Africa".'),
  ('MART', 'Martial Eagle', 'Polemaetus bellicosus',
   'The largest eagle in Africa, with a powerful build and dark brown plumage. A formidable predator.'),
  ('VEAG', 'Verreaux''s Eagle', 'Aquila verreauxii',
   'Large, black eagle with distinctive white markings on the back and wings. Specialises in hunting hyrax.'),
  ('WHBA', 'White-backed Vulture', 'Gyps africanus',
   'Medium-sized vulture with a distinctive white back. Highly social and often seen in large flocks at carcasses.'),
  ('LAPP', 'Lappet-faced Vulture', 'Torgos tracheliotos',
   'Large vulture with a massive bill and distinctive pink head. Dominant at carcasses.'),
  ('CVUL', 'Cape Vulture', 'Gyps coprotheres',
   'Large, creamy-white vulture endemic to southern Africa. Breeds on cliffs and is highly social.'),
  ('PELI', 'Great White Pelican', 'Pelecanus onocrotalus',
   'Large waterbird with a massive bill and throat pouch. Often seen in flocks on lakes and dams.'),
  ('FLAM', 'Greater Flamingo', 'Phoenicopterus roseus',
   'Large wading bird with pink plumage and long, curved neck. Filters algae and small organisms from water.'),
  ('HAMK', 'Hamerkop', 'Scopus umbretta',
   'Medium-sized wading bird with a distinctive hammer-shaped head. Builds enormous stick nests.'),
  ('HERO', 'Grey Heron', 'Ardea cinerea',
   'Large wading bird with grey plumage and a long, S-shaped neck. Stands motionless waiting for fish.'),
  ('CEGR', 'Cattle Egret', 'Bubulcus ibis',
   'Small, white egret often seen near livestock. Has a yellow bill and legs, turning orange in breeding season.'),
  ('HADB', 'Hadeda Ibis', 'Bostrychia hagedash',
   'Medium-sized ibis with a distinctive loud, raucous call. Common in gardens and parks.'),
  ('SACB', 'Sacred Ibis', 'Threskiornis aethiopicus',
   'White ibis with a bare black head and neck, and a long, curved bill. Often seen in wetlands.'),
  ('SPOO', 'Spur-winged Goose', 'Plectropterus gambensis',
   'The largest waterfowl in Africa, with black plumage and a distinctive red face. Has spurs on the wings.'),
  ('EGYT', 'Egyptian Goose', 'Alopochen aegyptiaca',
   'Medium-sized waterfowl with a distinctive brown eye patch and pink legs. Common on dams and rivers.'),
  ('KING', 'Pied Kingfisher', 'Ceryle rudis',
   'Black and white kingfisher that hovers over water before diving for fish. Common near rivers and dams.'),
  ('MALA', 'Malachite Kingfisher', 'Corythornis cristatus',
   'Tiny, brightly coloured kingfisher with a blue back and orange underparts. Found near reeds and overhanging vegetation.'),
  ('HORN', 'Southern Yellow-billed Hornbill', 'Tockus leucomelas',
   'Medium-sized hornbill with a distinctive yellow bill. Often seen in dry bush and savanna.'),
  ('GROU', 'Southern Ground Hornbill', 'Bucorvus leadbeateri',
   'Large, terrestrial hornbill with black plumage and a distinctive red face and throat. Endangered and highly territorial.'),
  ('LILI', 'Lilac-breasted Roller', 'Coracias caudatus',
   'Colourful bird with a lilac breast and blue wings. Often seen perched on branches, swooping to catch insects.'),
  ('PIED', 'Pied Crow', 'Corvus albus',
   'Black crow with a distinctive white chest and belly. Highly intelligent and adaptable.'),
  ('ROBI', 'Cape Robin-Chat', 'Cossypha caffra',
   'Small, grey bird with an orange breast and a distinctive white eye stripe. Known for its beautiful song.'),
  ('BULB', 'Cape Bulbul', 'Pycnonotus capensis',
   'Small, brown bird with a distinctive white eye ring. Common in gardens and fynbos.'),
  ('EYEB', 'Cape White-eye', 'Zosterops virens',
   'Small, greenish bird with a distinctive white eye ring. Active and social, often in small flocks.'),
  ('SUNB', 'Southern Double-collared Sunbird', 'Cinnyris chalybeus',
   'Small, nectar-feeding bird with iridescent green plumage in males. Common in fynbos and gardens.'),
  ('WEAV', 'Southern Masked Weaver', 'Ploceus velatus',
   'Small, yellow bird with a black face mask in males. Builds intricate woven nests.'),
  ('SPAR', 'House Sparrow', 'Passer domesticus',
   'Small, brown bird closely associated with human habitation. Introduced to South Africa.'),
  ('STAR', 'Common Starling', 'Sturnus vulgaris',
   'Small, dark bird with iridescent plumage. Introduced to South Africa and often seen in flocks.'),
  ('MYNA', 'Common Myna', 'Acridotheres tristis',
   'Medium-sized, brown bird with a yellow bill and legs. Introduced and invasive in South Africa.'),
  ('GUIN', 'Helmeted Guineafowl', 'Numida meleagris',
   'Ground-dwelling bird with grey plumage covered in white spots, and a distinctive bony casque on the head.'),
  ('FRAN', 'Grey-winged Francolin', 'Scleroptila afra',
   'Medium-sized game bird with grey-brown plumage. Found in grassland and fynbos.'),
  ('NAMA', 'Namaqua Dove', 'Oena capensis',
   'Tiny, delicate dove with a long, pointed tail. Males have a distinctive black face.'),
  ('LAUG', 'Laughing Dove', 'Spilopelia senegalensis',
   'Small, pinkish-brown dove with a distinctive laughing call. Common in gardens and parks.'),
  ('ROCK', 'Speckled Pigeon', 'Columba guinea',
   'Large, grey pigeon with distinctive white speckles on the wings. Common in rocky areas and buildings.'),
  ('TDOV', 'Cape Turtle Dove', 'Streptopelia capicola',
   'Medium-sized dove with a grey head and a distinctive black collar on the nape. Common in gardens.')
on conflict (code) do nothing;

-- ---------------------------------------------------------------------------
-- Reptiles
-- ---------------------------------------------------------------------------

insert into species (code, common_name, scientific_name, description) values
  ('CROC', 'Nile Crocodile', 'Crocodylus niloticus',
   'Large, aquatic reptile with a powerful tail and jaws. Ambush predator found in rivers, lakes and dams.'),
  ('LTOR', 'Leopard Tortoise', 'Stigmochelys pardalis',
   'Large tortoise with a distinctive domed shell patterned with black and yellow. Herbivorous and long-lived.'),
  ('SERR', 'Serrated Tortoise', 'Psammobates oculifer',
   'Small tortoise with a distinctive serrated shell edge. Found in dry, sandy areas.'),
  ('PANC', 'Pancake Tortoise', 'Malacochersus tornieri',
   'Small, flat tortoise with a flexible shell. Found in rocky outcrops.'),
  ('PUFF', 'Puff Adder', 'Bitis arietans',
   'Large, heavy-bodied viper with distinctive chevron markings. Responsible for most snakebite incidents in Africa.'),
  ('BOOM', 'Boomslang', 'Dispholidus typus',
   'Large, arboreal snake with large eyes and distinctive green or brown coloration. Highly venomous.'),
  ('MAMB', 'Black Mamba', 'Dendroaspis polylepis',
   'Large, fast-moving snake with a distinctive black mouth. One of Africa''s most feared snakes.'),
  ('GABO', 'Gaboon Viper', 'Bitis gabonica',
   'Large, heavy-bodied viper with distinctive geometric patterning. Has the longest fangs of any snake.'),
  ('NIGH', 'Night Adder', 'Causus rhombeatus',
   'Small, stout snake with distinctive V-shaped markings on the head. Nocturnal and mildly venomous.'),
  ('MOZA', 'Mozambique Spitting Cobra', 'Naja mossambica',
   'Medium-sized cobra capable of spitting venom accurately at eyes. Highly venomous.'),
  ('CCOB', 'Cape Cobra', 'Naja nivea',
   'Medium-sized cobra with a distinctive yellow or orange coloration. Highly venomous and common in the Western Cape.'),
  ('RINK', 'Rinkhals', 'Hemachatus haemachatus',
   'Medium-sized cobra-like snake with distinctive black and white bands. Can spit venom and plays dead when threatened.'),
  ('MONI', 'Water Monitor', 'Varanus niloticus',
   'Large, semi-aquatic lizard with a powerful tail. Excellent swimmer and opportunistic feeder.'),
  ('RMON', 'Rock Monitor', 'Varanus albigularis',
   'Large, terrestrial lizard with a powerful build. Found in rocky areas and savanna.'),
  ('CHAM', 'Cape Chameleon', 'Bradypodion pumilum',
   'Small, arboreal chameleon with the ability to change color. Slow-moving and found in fynbos and gardens.'),
  ('GECK', 'Common Flat Gecko', 'Afroedura nivaria',
   'Small, nocturnal gecko with a flattened body. Found under rocks and bark.'),
  ('SKIN', 'Rainbow Skink', 'Trachylepis margaritifera',
   'Small, shiny skink with iridescent scales. Common in rocky areas and gardens.'),
  ('LIZA', 'Rock Lizard', 'Nucras holubi',
   'Small, slender lizard with a long tail. Found in rocky areas and grassland.')
on conflict (code) do nothing;

-- ---------------------------------------------------------------------------
-- Amphibians
-- ---------------------------------------------------------------------------

insert into species (code, common_name, scientific_name, description) values
  ('BULL', 'African Bullfrog', 'Pyxicephalus adspersus',
   'Large, aggressive frog with a distinctive call. Males guard the tadpoles fiercely.'),
  ('REED', 'Reed Frog', 'Hyperolius marmoratus',
   'Small, brightly coloured tree frog with distinctive markings. Common in reeds and vegetation near water.'),
  ('TOAD', 'Guttural Toad', 'Sclerophrys gutturalis',
   'Medium-sized toad with distinctive parotoid glands. Common in gardens and near water.'),
  ('FROG', 'Common River Frog', 'Afrana angolensis',
   'Medium-sized frog found near rivers and streams. Has a distinctive call and is an important indicator species.')
on conflict (code) do nothing;

-- ---------------------------------------------------------------------------
-- Marine mammals (for coastal reserves)
-- ---------------------------------------------------------------------------

insert into species (code, common_name, scientific_name, description) values
  ('DOLP', 'Common Dolphin', 'Delphinus delphis',
   'Small, fast-swimming marine mammal with a distinctive hourglass pattern. Often seen in pods.'),
  ('HUMP', 'Humpback Whale', 'Megaptera novaeangliae',
   'Large baleen whale known for breaching and complex songs. Migrates along the South African coast.'),
  ('SOUT', 'Southern Right Whale', 'Eubalaena australis',
   'Large baleen whale that was nearly hunted to extinction. Now protected and seen off the coast in winter.'),
  ('DSEA', 'Cape Fur Seal', 'Arctocephalus pusillus',
   'Marine mammal with a thick fur coat. Found in colonies along the coast.'),
  ('SHAR', 'Great White Shark', 'Carcharodon carcharias',
   'Large, apex predator shark with distinctive counter-shading. Found in coastal waters.')
on conflict (code) do nothing;
