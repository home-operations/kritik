// Parses a unified `git diff` into files and numbered lines so the diff tab
// can anchor findings under the new-side line they point at.

export type LineKind = 'add' | 'del' | 'ctx' | 'hunk' | 'meta';

export interface DiffLine {
  kind: LineKind;
  text: string;
  oldNo: number | null;
  newNo: number | null;
}

export interface DiffFile {
  oldPath: string;
  newPath: string;
  // path is the side a reader identifies the file by: the new path, or the
  // old one for a deletion.
  path: string;
  header: string[];
  lines: DiffLine[];
  added: number;
  removed: number;
}

const HUNK = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/;

function stripPrefix(p: string): string {
  if (p === '/dev/null') return '';
  return p.replace(/^[ab]\//, '');
}

export function parseDiff(src: string): DiffFile[] {
  const files: DiffFile[] = [];
  let cur: DiffFile | undefined;
  let oldNo = 0;
  let newNo = 0;
  let inHunk = false;
  for (const text of src.split('\n')) {
    const git = /^diff --git a\/(.*) b\/(.*)$/.exec(text);
    if (git) {
      cur = { oldPath: git[1] ?? '', newPath: git[2] ?? '', path: git[2] ?? '', header: [text], lines: [], added: 0, removed: 0 };
      files.push(cur);
      inHunk = false;
      continue;
    }
    if (!cur) continue;
    const hunk = HUNK.exec(text);
    if (hunk) {
      oldNo = Number(hunk[1]);
      newNo = Number(hunk[2]);
      inHunk = true;
      cur.lines.push({ kind: 'hunk', text, oldNo: null, newNo: null });
      continue;
    }
    if (!inHunk) {
      if (text.startsWith('--- ')) cur.oldPath = stripPrefix(text.slice(4));
      else if (text.startsWith('+++ ')) cur.newPath = stripPrefix(text.slice(4));
      cur.path = cur.newPath || cur.oldPath;
      if (text !== '') cur.header.push(text);
      continue;
    }
    if (text.startsWith('+')) {
      cur.lines.push({ kind: 'add', text: text.slice(1), oldNo: null, newNo: newNo++ });
      cur.added++;
    } else if (text.startsWith('-')) {
      cur.lines.push({ kind: 'del', text: text.slice(1), oldNo: oldNo++, newNo: null });
      cur.removed++;
    } else if (text.startsWith(' ')) {
      cur.lines.push({ kind: 'ctx', text: text.slice(1), oldNo: oldNo++, newNo: newNo++ });
    } else if (text.startsWith('\\')) {
      cur.lines.push({ kind: 'meta', text, oldNo: null, newNo: null });
    }
  }
  return files;
}
