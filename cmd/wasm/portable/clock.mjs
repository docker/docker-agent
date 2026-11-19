// Browser counterparts of pkg/tools/builtin/{datetime,random} and the two prompt hooks.
const maxBytes = 64 * 1024;
const encoder = new TextEncoder();
const minInt = -(1n << 63n), maxInt = (1n << 63n) - 1n;
const jsonTokens = /"(?:[^"\\]|\\.)*"|-?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?|true|false|null|[[\]{}]/g;

function jsonType(token) {
  switch (token[0]) {
    case '{': return 'object';
    case '[': return 'array';
    case '"': return 'string';
    case 't': case 'f': return 'bool';
    case 'n': return 'null';
    default: return 'number';
  }
}

function decodeField(key, token, type, int) {
  if (!int && type === 'string') return JSON.parse(token).toWellFormed();
  if (int && type === 'number' && token.length <= 20 && /^-?\d+$/.test(token)) {
    const n = BigInt(token);
    if (n >= minInt && n <= maxInt) return n;
  }
  const source = int && type === 'number' ? `number ${token}` : type;
  throw new Error(`Error calling tool: json: cannot unmarshal ${source} into Go struct field Args.${key} of type ${int ? 'int64' : 'string'}`);
}

function argumentsFor(kind, raw) {
  if (typeof raw !== 'string') throw new Error('expected raw tool arguments');
  if (raw.length > maxBytes || encoder.encode(raw).length > maxBytes) throw new Error('clock argument size limit reached');
  if (!raw) return {};
  try { JSON.parse(raw); } catch (e) {
    const eof = /end of (?:JSON )?(?:input|data)/i.test(e.message) || e.message.includes(`position ${raw.length} `);
    throw new Error(`Error calling tool: ${eof ? 'unexpected end of JSON input' : 'invalid JSON input'}`);
  }
  const args = {};
  let depth = 0, key;
  // The JS object loses duplicate fields, wire order and the original numeric spelling.
  for (const [token] of raw.matchAll(jsonTokens)) {
    if (token === '}' || token === ']') { depth--; continue; }
    const type = jsonType(token);
    if (depth === 0 && type !== 'object' && type !== 'null') {
      throw new Error(`Error calling tool: json: cannot unmarshal ${type} into Go value of type ${kind}.Args`);
    }
    if (depth === 1) {
      if (key === undefined) { key = JSON.parse(token); continue; }
      const field = key.toLowerCase();
      const int = kind === 'random' && (field === 'min' || field === 'max');
      const str = kind === 'datetime' && (field === 'format' || field === 'timezone');
      if (int || str) {
        if (type === 'null') {
          if (int) delete args[field]; // null resets *int64, but leaves string fields alone.
        } else args[field] = decodeField(key, token, type, int);
      }
      key = undefined;
    }
    if ((type === 'object' || type === 'array') && ++depth > 10_000) {
      throw new Error('Error calling tool: exceeded max depth');
    }
  }
  return args;
}

function randomInt({ min, max }) {
  if (min == null || max == null) throw new Error('min and max are required');
  if (min > max) throw new Error('min must be less than or equal to max');
  if (min === max) return String(min);
  const width = max - min + 1n;
  const limit = (1n << 64n) / width * width;
  const draw = new BigUint64Array(1);
  try {
    // Reject the incomplete tail rather than giving some remainders a higher probability.
    do { crypto.getRandomValues(draw); } while (draw[0] >= limit);
  } catch (e) { throw new Error('failed to generate random integer: ' + e.message); }
  return String(min + draw[0] % width);
}

const months = 'January February March April May June July August September October November December'.split(' ');
const weekdays = 'Sunday Monday Tuesday Wednesday Thursday Friday Saturday'.split(' ');
const pad = (n, width = 2) => String(n).padStart(width, '0');
const layoutTokens = /January|Jan(?![a-z])|Monday|Mon(?![a-z])|MST|_2006|2006|__2|_2|002|0[1-6]|15|[1-5]|PM|pm|[Z-]07(?::00:00|0000|:00|00)?|[.,](?:0+|9+)(?!\d)/g;

function formatLayout(layout, date, offset, name) {
  const year = date.getUTCFullYear(), month = date.getUTCMonth(), day = date.getUTCDate();
  const hour = date.getUTCHours(), minute = date.getUTCMinutes(), second = date.getUTCSeconds();
  const jan1 = new Date(0);
  jan1.setUTCFullYear(year, 0, 1);
  const yday = 1 + Math.floor((date - jan1) / 86400000);
  const meridiem = hour < 12 ? 'AM' : 'PM', weekday = weekdays[date.getUTCDay()];
  const values = {
    January: months[month], Jan: months[month].slice(0, 3), Monday: weekday, Mon: weekday.slice(0, 3),
    '2006': pad(year, 4), _2006: '_' + pad(year, 4), '06': pad(year % 100), '1': month + 1, '01': pad(month + 1),
    '2': day, '02': pad(day), _2: String(day).padStart(2), '002': pad(yday, 3), __2: String(yday).padStart(3),
    '15': pad(hour), '3': hour % 12 || 12, '03': pad(hour % 12 || 12), '4': minute, '04': pad(minute),
    '5': second, '05': pad(second), PM: meridiem, pm: meridiem.toLowerCase(), MST: name,
  };
  const fraction = pad(date.getUTCMilliseconds(), 3) + '000000'; // Go js/wasm walltime also reads Date in milliseconds.
  const abs = Math.abs(offset), signHour = (offset < 0 ? '-' : '+') + pad(Math.floor(abs / 3600));
  const minutes = pad(Math.floor(abs / 60) % 60), seconds = pad(abs % 60);
  return layout.replace(layoutTokens, token => {
    if (Object.hasOwn(values, token)) return values[token];
    if (token[0] === '.' || token[0] === ',') {
      const digits = fraction.slice(0, (token.length - 1) & 0xfff); // Go packs the requested precision into 12 bits.
      if (token[1] === '0') return token[0] + digits;
      const trimmed = digits.replace(/0+$/, '');
      return trimmed && token[0] + trimmed;
    }
    if (token[0] === 'Z' && !offset) return 'Z';
    const sep = token.includes(':') ? ':' : '';
    return signHour + (token.length > 3 ? sep + minutes : '') + (token.length > 6 ? sep + seconds : '');
  });
}

// Intl resolves transitions, but accepts non-IANA names and localizes abbreviations (e.g. JST as GMT+9).
// Group Go's lib/time/zoneinfo.zip names by their current TZif footer: standard name, optional DST name
// and standard offset in minutes. '-' denotes a numeric IANA abbreviation.
const zoneNames = new Map();
for (const [standard, summer, standardOffset, names] of [
  ['-', '', null, `Africa/Casablanca Africa/El_Aaiun America/Araguaina America/Argentina/Buenos_Aires America/Argentina/Catamarca
America/Argentina/ComodRivadavia America/Argentina/Cordoba America/Argentina/Jujuy America/Argentina/La_Rioja
America/Argentina/Mendoza America/Argentina/Rio_Gallegos America/Argentina/Salta America/Argentina/San_Juan
America/Argentina/San_Luis America/Argentina/Tucuman America/Argentina/Ushuaia America/Asuncion America/Bahia
America/Belem America/Boa_Vista America/Bogota America/Buenos_Aires America/Campo_Grande America/Caracas
America/Catamarca America/Cayenne America/Cordoba America/Coyhaique America/Cuiaba America/Eirunepe
America/Fortaleza America/Godthab America/Guayaquil America/Guyana America/Jujuy America/La_Paz America/Lima
America/Maceio America/Manaus America/Mendoza America/Miquelon America/Montevideo America/Noronha America/Nuuk
America/Paramaribo America/Porto_Acre America/Porto_Velho America/Punta_Arenas America/Recife
America/Rio_Branco America/Rosario America/Santarem America/Santiago America/Sao_Paulo America/Scoresbysund
Antarctica/Casey Antarctica/Davis Antarctica/DumontDUrville Antarctica/Mawson Antarctica/Palmer
Antarctica/Rothera Antarctica/Syowa Antarctica/Troll Antarctica/Vostok Asia/Aden Asia/Almaty Asia/Amman
Asia/Anadyr Asia/Aqtau Asia/Aqtobe Asia/Ashgabat Asia/Ashkhabad Asia/Atyrau Asia/Baghdad Asia/Bahrain
Asia/Baku Asia/Bangkok Asia/Barnaul Asia/Bishkek Asia/Brunei Asia/Chita Asia/Choibalsan Asia/Colombo
Asia/Dacca Asia/Damascus Asia/Dhaka Asia/Dili Asia/Dubai Asia/Dushanbe Asia/Ho_Chi_Minh Asia/Hovd Asia/Irkutsk
Asia/Istanbul Asia/Kabul Asia/Kamchatka Asia/Kashgar Asia/Kathmandu Asia/Katmandu Asia/Khandyga
Asia/Krasnoyarsk Asia/Kuala_Lumpur Asia/Kuching Asia/Kuwait Asia/Magadan Asia/Muscat Asia/Novokuznetsk
Asia/Novosibirsk Asia/Omsk Asia/Oral Asia/Phnom_Penh Asia/Qatar Asia/Qostanay Asia/Qyzylorda Asia/Rangoon
Asia/Riyadh Asia/Saigon Asia/Sakhalin Asia/Samarkand Asia/Singapore Asia/Srednekolymsk Asia/Tashkent
Asia/Tbilisi Asia/Tehran Asia/Thimbu Asia/Thimphu Asia/Tomsk Asia/Ulaanbaatar Asia/Ulan_Bator Asia/Urumqi
Asia/Ust-Nera Asia/Vientiane Asia/Vladivostok Asia/Yakutsk Asia/Yangon Asia/Yekaterinburg Asia/Yerevan
Atlantic/Azores Atlantic/Cape_Verde Atlantic/South_Georgia Atlantic/Stanley Australia/Eucla Australia/LHI
Australia/Lord_Howe Brazil/Acre Brazil/DeNoronha Brazil/East Brazil/West Chile/Continental Chile/EasterIsland
Etc/GMT+1 Etc/GMT+10 Etc/GMT+11 Etc/GMT+12 Etc/GMT+2 Etc/GMT+3 Etc/GMT+4 Etc/GMT+5 Etc/GMT+6 Etc/GMT+7
Etc/GMT+8 Etc/GMT+9 Etc/GMT-1 Etc/GMT-10 Etc/GMT-11 Etc/GMT-12 Etc/GMT-13 Etc/GMT-14 Etc/GMT-2 Etc/GMT-3
Etc/GMT-4 Etc/GMT-5 Etc/GMT-6 Etc/GMT-7 Etc/GMT-8 Etc/GMT-9 Europe/Astrakhan Europe/Istanbul Europe/Minsk
Europe/Samara Europe/Saratov Europe/Ulyanovsk Indian/Chagos Indian/Christmas Indian/Cocos Indian/Kerguelen
Indian/Mahe Indian/Maldives Indian/Mauritius Indian/Reunion Iran Kwajalein NZ-CHAT Pacific/Apia
Pacific/Bougainville Pacific/Chatham Pacific/Chuuk Pacific/Easter Pacific/Efate Pacific/Enderbury
Pacific/Fakaofo Pacific/Fiji Pacific/Funafuti Pacific/Galapagos Pacific/Gambier Pacific/Guadalcanal
Pacific/Kanton Pacific/Kiritimati Pacific/Kosrae Pacific/Kwajalein Pacific/Majuro Pacific/Marquesas
Pacific/Nauru Pacific/Niue Pacific/Norfolk Pacific/Noumea Pacific/Palau Pacific/Pitcairn Pacific/Pohnpei
Pacific/Ponape Pacific/Port_Moresby Pacific/Rarotonga Pacific/Tahiti Pacific/Tarawa Pacific/Tongatapu
Pacific/Truk Pacific/Wake Pacific/Wallis Pacific/Yap Singapore Turkey`],
  ['-00', '', null, `Factory`],
  ['ACST', '', null, `Australia/Darwin Australia/North`],
  ['ACST', 'ACDT', 570, `Australia/Adelaide Australia/Broken_Hill Australia/South Australia/Yancowinna`],
  ['AEST', '', null, `Australia/Brisbane Australia/Lindeman Australia/Queensland`],
  ['AEST', 'AEDT', 600, `Antarctica/Macquarie Australia/ACT Australia/Canberra Australia/Currie Australia/Hobart Australia/Melbourne
Australia/NSW Australia/Sydney Australia/Tasmania Australia/Victoria`],
  ['AKST', 'AKDT', -540, `America/Anchorage America/Juneau America/Metlakatla America/Nome America/Sitka America/Yakutat US/Alaska`],
  ['AST', '', null, `America/Anguilla America/Antigua America/Aruba America/Barbados America/Blanc-Sablon America/Curacao
America/Dominica America/Grenada America/Guadeloupe America/Kralendijk America/Lower_Princes America/Marigot
America/Martinique America/Montserrat America/Port_of_Spain America/Puerto_Rico America/Santo_Domingo
America/St_Barthelemy America/St_Kitts America/St_Lucia America/St_Thomas America/St_Vincent America/Tortola
America/Virgin`],
  ['AST', 'ADT', -240, `America/Glace_Bay America/Goose_Bay America/Halifax America/Moncton America/Thule Atlantic/Bermuda
Canada/Atlantic`],
  ['AWST', '', null, `Australia/Perth Australia/West`],
  ['CAT', '', null, `Africa/Blantyre Africa/Bujumbura Africa/Gaborone Africa/Harare Africa/Juba Africa/Khartoum Africa/Kigali
Africa/Lubumbashi Africa/Lusaka Africa/Maputo Africa/Windhoek`],
  ['CET', '', null, `Africa/Algiers Africa/Tunis`],
  ['CET', 'CEST', 60, `Africa/Ceuta Arctic/Longyearbyen Atlantic/Jan_Mayen CET Europe/Amsterdam Europe/Andorra Europe/Belgrade
Europe/Berlin Europe/Bratislava Europe/Brussels Europe/Budapest Europe/Busingen Europe/Copenhagen
Europe/Gibraltar Europe/Ljubljana Europe/Luxembourg Europe/Madrid Europe/Malta Europe/Monaco Europe/Oslo
Europe/Paris Europe/Podgorica Europe/Prague Europe/Rome Europe/San_Marino Europe/Sarajevo Europe/Skopje
Europe/Stockholm Europe/Tirane Europe/Vaduz Europe/Vatican Europe/Vienna Europe/Warsaw Europe/Zagreb
Europe/Zurich MET Poland`],
  ['CST', '', null, `America/Bahia_Banderas America/Belize America/Chihuahua America/Costa_Rica America/Edmonton
America/El_Salvador America/Guatemala America/Managua America/Merida America/Mexico_City America/Monterrey
America/Regina America/Swift_Current America/Tegucigalpa America/Yellowknife Asia/Chongqing Asia/Chungking
Asia/Harbin Asia/Macao Asia/Macau Asia/Shanghai Asia/Taipei Canada/Mountain Canada/Saskatchewan Mexico/General
PRC ROC`],
  ['CST', 'CDT', -360, `America/Chicago America/Indiana/Knox America/Indiana/Tell_City America/Knox_IN America/Matamoros
America/Menominee America/North_Dakota/Beulah America/North_Dakota/Center America/North_Dakota/New_Salem
America/Ojinaga America/Rainy_River America/Rankin_Inlet America/Resolute America/Winnipeg CST6CDT
Canada/Central US/Central US/Indiana-Starke`],
  ['CST', 'CDT', -300, `America/Havana Cuba`],
  ['ChST', '', null, `Pacific/Guam Pacific/Saipan`],
  ['EAT', '', null, `Africa/Addis_Ababa Africa/Asmara Africa/Asmera Africa/Dar_es_Salaam Africa/Djibouti Africa/Kampala
Africa/Mogadishu Africa/Nairobi Indian/Antananarivo Indian/Comoro Indian/Mayotte`],
  ['EET', '', null, `Africa/Tripoli Europe/Kaliningrad Libya`],
  ['EET', 'EEST', 120, `Africa/Cairo Asia/Beirut Asia/Famagusta Asia/Gaza Asia/Hebron Asia/Nicosia EET Egypt Europe/Athens
Europe/Bucharest Europe/Chisinau Europe/Helsinki Europe/Kiev Europe/Kyiv Europe/Mariehamn Europe/Nicosia
Europe/Riga Europe/Sofia Europe/Tallinn Europe/Tiraspol Europe/Uzhgorod Europe/Vilnius Europe/Zaporozhye`],
  ['EST', '', null, `America/Atikokan America/Cancun America/Cayman America/Coral_Harbour America/Jamaica America/Panama EST
Jamaica`],
  ['EST', 'EDT', -300, `America/Detroit America/Fort_Wayne America/Grand_Turk America/Indiana/Indianapolis America/Indiana/Marengo
America/Indiana/Petersburg America/Indiana/Vevay America/Indiana/Vincennes America/Indiana/Winamac
America/Indianapolis America/Iqaluit America/Kentucky/Louisville America/Kentucky/Monticello
America/Louisville America/Montreal America/Nassau America/New_York America/Nipigon America/Pangnirtung
America/Port-au-Prince America/Thunder_Bay America/Toronto Canada/Eastern EST5EDT US/East-Indiana US/Eastern
US/Michigan`],
  ['GMT', '', null, `Africa/Abidjan Africa/Accra Africa/Bamako Africa/Banjul Africa/Bissau Africa/Conakry Africa/Dakar
Africa/Freetown Africa/Lome Africa/Monrovia Africa/Nouakchott Africa/Ouagadougou Africa/Sao_Tome
Africa/Timbuktu America/Danmarkshavn Atlantic/Reykjavik Atlantic/St_Helena Etc/GMT Etc/GMT+0 Etc/GMT-0
Etc/GMT0 Etc/Greenwich GMT GMT+0 GMT-0 GMT0 Greenwich Iceland`],
  ['GMT', 'BST', 0, `Europe/Belfast Europe/Guernsey Europe/Isle_of_Man Europe/Jersey Europe/London GB GB-Eire`],
  ['HKT', '', null, `Asia/Hong_Kong Hongkong`],
  ['HST', '', null, `HST Pacific/Honolulu Pacific/Johnston US/Hawaii`],
  ['HST', 'HDT', -600, `America/Adak America/Atka US/Aleutian`],
  ['IST', '', null, `Asia/Calcutta Asia/Kolkata`],
  ['IST', 'GMT', 60, `Eire Europe/Dublin`],
  ['IST', 'IDT', 120, `Asia/Jerusalem Asia/Tel_Aviv Israel`],
  ['JST', '', null, `Asia/Tokyo Japan`],
  ['KST', '', null, `Asia/Pyongyang Asia/Seoul ROK`],
  ['MSK', '', null, `Europe/Kirov Europe/Moscow Europe/Simferopol Europe/Volgograd W-SU`],
  ['MST', '', null, `America/Creston America/Dawson America/Dawson_Creek America/Fort_Nelson America/Hermosillo America/Mazatlan
America/Phoenix America/Vancouver America/Whitehorse Canada/Pacific Canada/Yukon MST Mexico/BajaSur US/Arizona`],
  ['MST', 'MDT', -420, `America/Boise America/Cambridge_Bay America/Ciudad_Juarez America/Denver America/Inuvik America/Shiprock
MST7MDT Navajo US/Mountain`],
  ['NST', 'NDT', -210, `America/St_Johns Canada/Newfoundland`],
  ['NZST', 'NZDT', 720, `Antarctica/McMurdo Antarctica/South_Pole NZ Pacific/Auckland`],
  ['PKT', '', null, `Asia/Karachi`],
  ['PST', '', null, `Asia/Manila`],
  ['PST', 'PDT', -480, `America/Ensenada America/Los_Angeles America/Santa_Isabel America/Tijuana Mexico/BajaNorte PST8PDT US/Pacific`],
  ['SAST', '', null, `Africa/Johannesburg Africa/Maseru Africa/Mbabane`],
  ['SST', '', null, `Pacific/Midway Pacific/Pago_Pago Pacific/Samoa US/Samoa`],
  ['UTC', '', null, `Etc/UCT Etc/UTC Etc/Universal Etc/Zulu UCT UTC Universal Zulu`],
  ['WAT', '', null, `Africa/Bangui Africa/Brazzaville Africa/Douala Africa/Kinshasa Africa/Lagos Africa/Libreville Africa/Luanda
Africa/Malabo Africa/Ndjamena Africa/Niamey Africa/Porto-Novo`],
  ['WET', 'WEST', 0, `Atlantic/Canary Atlantic/Faeroe Atlantic/Faroe Atlantic/Madeira Europe/Lisbon Portugal WET`],
  ['WIB', '', null, `Asia/Jakarta Asia/Pontianak`],
  ['WIT', '', null, `Asia/Jayapura`],
  ['WITA', '', null, `Asia/Makassar Asia/Ujung_Pandang`],
]) {
  const rule = { standard, summer, standardOffset };
  for (const zone of names.split(/\s+/)) zoneNames.set(zone, rule);
}

function quoteZone(zone) {
  return '"' + zone.replace(/["\\]|[^\p{L}\p{M}\p{N}\p{P}\p{S} ]/gu, ch => {
    if (ch === '\x07') return '\\a';
    if (ch === '\v') return '\\v';
    if ('"\\\b\f\n\r\t'.includes(ch)) return JSON.stringify(ch).slice(1, -1);
    const n = ch.codePointAt(0), hex = n.toString(16);
    return n < 128 ? '\\x' + pad(hex) : n < 65536 ? '\\u' + pad(hex, 4) : '\\U' + pad(hex, 8);
  }) + '"';
}

function zoneAt(zone, now) {
  const local = !zone || zone === 'Local';
  try {
    if (local) zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    const rule = zoneNames.get(zone);
    if (!rule) throw new RangeError();
    if (rule.standard === 'UTC' || zone === 'Factory') return { offset: 0, name: rule.standard };
    const fmt = new Intl.DateTimeFormat('en-US', { timeZone: zone, timeZoneName: 'longOffset' });
    const text = fmt.formatToParts(now).find(p => p.type === 'timeZoneName').value;
    const [, sign, hh, mm, ss] = /^GMT(?:([+-])(\d\d):(\d\d)(?::(\d\d))?)?$/.exec(text);
    const offset = ((Number(hh || 0) * 60 + Number(mm || 0)) * 60 + Number(ss || 0)) * (sign === '-' ? -1 : 1);
    const name = rule.standard === '-' ? `${sign || '+'}${hh || '00'}${Number(mm) ? mm : ''}`
      : rule.summer && offset !== rule.standardOffset * 60 ? rule.summer : rule.standard;
    return { offset, name };
  } catch {
    if (local) return { offset: -now.getTimezoneOffset() * 60, name: 'Local' };
    const error = zone.includes('..') || /^[\\/]/.test(zone) ? 'time: invalid location name'
      : zone.includes('\0') ? 'Invalid argument' : 'not implemented on js'; // wasm_exec's filesystem stub.
    throw new Error(`Error: invalid timezone ${quoteZone(zone)}: ${error}`);
  }
}

function datetime({ format, timezone }) {
  if (!format || /^\p{White_Space}+$/u.test(format)) {
    throw new Error('Error: format is required; use a Go time layout such as 2006-01-02 or 15:04:05.');
  }
  const now = new Date();
  const { offset, name } = zoneAt(timezone, now);
  return formatLayout(format, new Date(+now + offset * 1000), offset, name);
}

export function clockTool(name, argsJSON) {
  try {
    let output;
    switch (name) {
      case 'random_int': output = randomInt(argumentsFor('random', argsJSON)); break;
      case 'get_datetime': output = datetime(argumentsFor('datetime', argsJSON)); break;
      default: throw new Error('unknown clock tool');
    }
    return { output, is_error: false };
  } catch (e) { return { output: e.message, is_error: true }; }
}

const browserEnvironment = `Here is useful information about the environment you are running in:
\t<env>
\tWorking directory: /
\tOperating System: js
\tCPU Architecture: wasm
\t</env>`;

let hookTimezoneOffset;

// Insert these strings after invariant instructions, before history and provider-specific merging.
export function dynamicPrompt(flags, meta = {}) {
  const messages = [];
  if (flags.add_environment_info) messages.push(browserEnvironment);
  if (flags.add_date) {
    const now = meta.now == null ? new Date() : new Date(meta.now);
    // Unlike the datetime tool, Go's time.Local on wasm captures a single offset.
    const offset = meta.timezoneOffset ?? (hookTimezoneOffset ??= now.getTimezoneOffset());
    const date = new Date(+now - offset * 60_000);
    messages.push(`Today's date: ${pad(date.getUTCFullYear(), 4)}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())}`);
  }
  return messages;
}
