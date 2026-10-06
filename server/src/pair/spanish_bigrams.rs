// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

pub(crate) const LETTER_PAIR_ROWS: [(&str, &[&str]); 26] = [
    (
        "a",
        &[
            "r", "n", "s", "l", "d", "c", "t", "m", "b", "v", "p", "g", "z", "j", "f", "q", "y",
            "i", "u", "h", "o", "x", "k", "e", "w$",
        ],
    ),
    (
        "b",
        &["o", "a", "ie", "lr", "uys", "dnw", "gctk", "hfjm", "pqvxz$"],
    ),
    (
        "c",
        &["o", "a", "he", "ik", "rtu", "lys", "nvqm", "gbdf", "jpwxz$"],
    ),
    (
        "d",
        &["o", "a", "ei", "ur", "syh", "wtl", "vkcb", "nfjp", "gzmqx$"],
    ),
    (
        "e",
        &[
            "s", "n", "r", "lc", "tf", "ma", "gix", "dzp", "jbvoy", "qukwh$",
        ],
    ),
    (
        "f",
        &["o", "r", "ie", "au", "ltp", "ysh", "vdnb", "cgjk", "mqwxz$"],
    ),
    (
        "g",
        &["a", "e", "ri", "uo", "lhn", "syd", "mpvx", "btzf", "kcjqw$"],
    ),
    (
        "h",
        &["a", "o", "ei", "ut", "ynw", "lrm", "bsfk", "cdgj", "pqvxz$"],
    ),
    (
        "i",
        &[
            "n", "a", "c", "dl", "to", "em", "svp", "gzb", "rfkqx", "ujwhy$",
        ],
    ),
    (
        "j",
        &["a", "o", "ie", "um", "bcd", "fgh", "klnp", "qrst", "vwxyz$"],
    ),
    (
        "k",
        &["i", "e", "as", "oy", "rut", "ngf", "lwhm", "dcpj", "bqvxz$"],
    ),
    (
        "l",
        &["a", "o", "ei", "us", "tcm", "dvg", "bpyk", "fhzw", "nqxrj$"],
    ),
    (
        "m",
        &["a", "e", "oi", "pb", "usy", "ftk", "vcgr", "dnhl", "jqwxz$"],
    ),
    (
        "n",
        &["a", "t", "oe", "id", "csg", "uzv", "jfkq", "byrl", "mhpwx$"],
    ),
    (
        "o",
        &["n", "r", "ls", "ut", "cmg", "jde", "pbxv", "afzh", "iyqwk$"],
    ),
    (
        "p",
        &["a", "i", "er", "ol", "ush", "tyf", "qzbc", "dvnj", "mwgkx$"],
    ),
    (
        "q",
        &["u", "s", "ac", "bd", "efg", "hij", "klmn", "oprt", "vwxyz$"],
    ),
    (
        "r",
        &["a", "e", "io", "tu", "dmg", "vnb", "csfp", "lyqk", "zhjwx$"],
    ),
    (
        "s",
        &["a", "i", "ot", "ec", "puq", "hnm", "kdly", "fwbg", "vrxjz$"],
    ),
    (
        "t",
        &["e", "a", "io", "ru", "lsh", "yck", "wxnp", "dmzf", "gbjqv$"],
    ),
    (
        "u",
        &["r", "e", "nl", "ts", "cia", "mdb", "pgjz", "ovyf", "kxqhw$"],
    ),
    (
        "v",
        &["e", "a", "io", "uc", "lnt", "dry", "smbf", "ghjk", "pqwxz$"],
    ),
    (
        "w",
        &["i", "h", "e", "ao", "nubys", "rdtlk", "cfgjm", "pqvxz$"],
    ),
    (
        "x",
        &["t", "i", "p", "ea", "oc", "fyu", "bslhq", "zkmrd", "gjnvw$"],
    ),
    (
        "y",
        &["o", "a", "e", "bs", "dlntu", "cprmz", "gxifv", "kwhjq$"],
    ),
    (
        "z",
        &["a", "u", "oe", "ci", "psm", "nky", "tqhl", "bdfg", "jrvwx$"],
    ),
];
