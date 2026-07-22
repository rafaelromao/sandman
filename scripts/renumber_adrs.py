#!/usr/bin/env python3
"""Renumber ADR files to fill gaps left by deleted ADRs, and update cross-references."""
import os
import re
import shutil

ADRS_DIR = "docs/adr"
DELETED = {"0012", "0017", "0022", "0023"}

OLD_TO_NEW = {
    "0001": "0001", "0002": "0002", "0003": "0003", "0004": "0004",
    "0005": "0005", "0006": "0006", "0007": "0007", "0008": "0008",
    "0009": "0009", "0010": "0010", "0011": "0011",
    "0013": "0012", "0014": "0013", "0015": "0014", "0016": "0015",
    "0018": "0016", "0019": "0017", "0020": "0018", "0021": "0019",
    "0024": "0020", "0025": "0021", "0026": "0022", "0027": "0023",
    "0028": "0024", "0029": "0025", "0030": "0026", "0031": "0027",
    "0033": "0028", "0034": "0029", "0035": "0030", "0036": "0031",
    "0037": "0032", "0039": "0033", "0040": "0034",
}

def process_content(content, old_num, new_num):
    parts = content.split('\n\n', 1)
    title = parts[0]
    body = parts[1] if len(parts) > 1 else ''

    def replace_ref(m):
        ref_old = m.group(1)
        ref_new = OLD_TO_NEW.get(ref_old)
        if ref_new:
            return f'ADR-{ref_new}'
        if ref_old in DELETED:
            return f'~~ADR-{ref_old} (deleted)~~'
        return m.group(0)

    title = re.sub(rf'^# ADR-{old_num}:', f'# ADR-{new_num}:', title)
    body = re.sub(r'ADR-(\d{4})', replace_ref, body)
    return title + '\n\n' + body

def main():
    files = sorted([f for f in os.listdir(ADRS_DIR)
                    if f.endswith(".md") and f != "README.md"],
                   reverse=True)

    renames = []
    for f in files:
        old_num = f.split("-")[0]
        if old_num in DELETED:
            print(f"  SKIP (deleted): {f}")
            continue
        new_num = OLD_TO_NEW.get(old_num)
        if new_num is None:
            print(f"  SKIP (no mapping): {f}")
            continue
        slug = "-".join(f.split("-")[1:])
        new_fname = f"{new_num}-{slug}"
        renames.append((f, old_num, new_num, new_fname))

    tmpdir = os.path.join(ADRS_DIR, "_renumber_tmp")
    if os.path.exists(tmpdir):
        shutil.rmtree(tmpdir)
    os.makedirs(tmpdir)

    old_to_tmp = {}
    for i, (old_fname, old_num, new_num, new_fname) in enumerate(renames):
        tmp_name = f"_rt_{i:04d}.md"
        src = os.path.join(ADRS_DIR, old_fname)
        dst = os.path.join(tmpdir, tmp_name)
        shutil.move(src, dst)
        old_to_tmp[old_fname] = (tmp_name, old_num, new_num, new_fname)
        if old_num == new_num:
            print(f"  KEEP: {old_fname}")
        else:
            print(f"  {old_num}→{new_num}: {old_fname} → {new_fname}")

    for old_fname, (tmp_name, old_num, new_num, new_fname) in old_to_tmp.items():
        tmp_path = os.path.join(tmpdir, tmp_name)
        final_path = os.path.join(ADRS_DIR, new_fname)
        with open(tmp_path, "r") as fh:
            content = fh.read()
        content = process_content(content, old_num, new_num)
        with open(final_path, "w") as fh:
            fh.write(content)

    shutil.rmtree(tmpdir)
    print(f"\nProcessed {len(renames)} files")

if __name__ == "__main__":
    main()
