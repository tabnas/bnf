// Copyright (c) 2026 tabnas, MIT License

//! The schema `grammar-rules`, held to the shared fixtures in
//! `test/spec/rules.tsv`, which `ts/test/rules.test.js` and
//! `go/rules_test.go` run too: each tree is the schema's or refused with the
//! code its row names. `serde_json` keeps an object's members in their
//! order (`preserve_order`), so the order is held as a front end's is.

#[test]
fn rules_spec() {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../test/spec/rules.tsv");
    let data = std::fs::read_to_string(&path).expect("test/spec/rules.tsv is readable");
    let mut rows = 0;
    for (i, line) in data.lines().enumerate().skip(1) {
        if line.is_empty() || (line.starts_with('#') && !line.contains('\t')) {
            continue;
        }
        let (input, expected) = line.split_once('\t').expect("a row has two cells");
        rows += 1;
        let tree: serde_json::Value = serde_json::from_str(input).expect("the input is JSON");
        match (expected, tabnas_bnf::check_rules(&tree)) {
            ("OK", Ok(())) => {}
            ("OK", Err(e)) => panic!("rules.tsv:{}: {e}", i + 1),
            (want, Ok(())) => panic!("rules.tsv:{}: want {want}, the tree passed", i + 1),
            (want, Err(e)) => assert_eq!(
                format!("ERROR:{}", e.code),
                want,
                "rules.tsv:{}: {e}",
                i + 1
            ),
        }
    }
    assert_eq!(rows, 35, "rules.tsv holds {rows} rows, not 35");
}
