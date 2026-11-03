import json, os, sys
P = os.path.join(os.path.dirname(__file__), "..", "..", "mojoprojects")
done = set(os.listdir(os.path.join(P, "state", "done"))) if os.path.isdir(os.path.join(P, "state", "done")) else set()
out = []
for fn, kind in (("cloud.tsv", "primitive"), ("biz.tsv", "business")):
    for line in open(os.path.join(P, fn)):
        f = line.rstrip("\n").split("\t")
        if len(f) < 3:
            continue
        slug, title, pitch = f[0], f[1], f[2]
        parts = f[3].split(",") if len(f) > 3 and f[3] else ([slug] if kind == "primitive" else [])
        repo = ("mojocloud-" if kind == "primitive" else "mojobiz-") + slug
        out.append({"slug": slug, "title": title, "pitch": pitch, "kind": kind, "parts": parts,
                    "repo": "https://github.com/lee101/" + repo,
                    "status": "built" if slug in done else "building"})
out.append({"slug": "how", "title": "how.nz agent",
            "pitch": "Ask anything: a Mojo agent that searches, reads and draws the answer, streamed over SSE; flat run fee plus per-token pricing",
            "kind": "product", "parts": ["how"], "repo": "https://github.com/lee101/hownz",
            "status": "built", "upstream": "http://127.0.0.1:4350"})
json.dump(out, open(os.path.join(os.path.dirname(__file__), "..", "businesses.json"), "w"), indent=1)
print(len(out), sum(o["status"] == "built" for o in out))
