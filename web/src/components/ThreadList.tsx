import type { ReactElement } from "react";
import { useState } from "react";
import type { Comment, Thread } from "../gen/loam/v1/common_pb";
import { Markdown } from "./Markdown";
import { StatusBadge } from "./StatusBadge";
import styles from "./ThreadList.module.css";

/** Threads sharing an anchor file. `file` is undefined for unanchored threads. */
export interface ThreadGroup {
  readonly file: string | undefined;
  readonly threads: readonly Thread[];
}

/** A run of consecutive comments that were all posted in the same round. */
export interface RoundRun {
  readonly round: number;
  readonly comments: readonly Comment[];
}

/**
 * Groups threads by their anchor's file.
 *
 * This is the ONE cross-thread arrangement the data model actually supports:
 * two threads anchored on the same file are related in the way a reviewer
 * thinks ("what did they say about this file"), and that relationship is read
 * off `Thread.anchor`, not guessed at.
 *
 * Unanchored threads come first as a single group -- they are about the
 * proposal as a whole, so they are the frame for everything below them.
 * Files then appear in the order the server sent them, NOT alphabetically:
 * any other order would be this component's invention rather than the
 * server's, and there is no ordering in the data to appeal to.
 *
 * Within a file, threads sort by anchor line ascending, whole-file anchors
 * first, with ties left in server order (a stable sort). One caveat worth
 * knowing, from loam-hi5o.24: an anchor records the line as it was when the
 * thread was RAISED and a thread cannot be re-anchored, so a fix round that
 * rewrote the file leaves an old thread sorted against a line number that no
 * longer means what it did. Each thread therefore shows the round it was
 * raised in, so the ordering is never the only thing a reviewer has.
 */
export function groupThreadsByAnchor(threads: readonly Thread[]): readonly ThreadGroup[] {
  const unanchored: Thread[] = [];
  const byFile = new Map<string, Thread[]>();
  for (const thread of threads) {
    const file = thread.anchor?.file;
    if (file === undefined) {
      unanchored.push(thread);
      continue;
    }
    const existing = byFile.get(file);
    if (existing === undefined) byFile.set(file, [thread]);
    else existing.push(thread);
  }
  const line = (thread: Thread): number => thread.anchor?.line ?? -1;
  const groups: ThreadGroup[] = [];
  if (unanchored.length > 0) groups.push({ file: undefined, threads: unanchored });
  for (const [file, group] of byFile) {
    groups.push({ file, threads: [...group].sort((a, b) => line(a) - line(b)) });
  }
  return groups;
}

/**
 * Splits a thread's comments into consecutive same-round runs, preserving
 * order exactly.
 *
 * `Comment.round` is the comment's OWN round and can be LATER than the
 * thread's (proto/loam/v1/common.proto:76-79 says so explicitly), so a thread
 * raised in round 1 with a reply in round 3 is a conversation that developed
 * across time. This is the derivation that makes that visible.
 *
 * It never reorders and never merges non-adjacent runs: two round-1 comments
 * either side of a round-2 one stay as three runs, because collapsing them
 * would rewrite the sequence into something the server did not send.
 */
export function roundRuns(comments: readonly Comment[]): readonly RoundRun[] {
  const runs: { round: number; comments: Comment[] }[] = [];
  for (const comment of comments) {
    const last = runs[runs.length - 1];
    if (last !== undefined && last.round === comment.round) last.comments.push(comment);
    else runs.push({ round: comment.round, comments: [comment] });
  }
  return runs;
}

/** A thread's anchor as a location, or the unanchored label. */
export function anchorLabel(thread: Thread): string {
  if (thread.anchor === undefined) return "General comment";
  return thread.anchor.line === undefined
    ? thread.anchor.file
    : `${thread.anchor.file}:${thread.anchor.line}`;
}

export interface ThreadListProps {
  readonly threads: readonly Thread[];
}

/**
 * ThreadList renders a work branch's comment threads (loam-ba6a.4).
 *
 * WHAT IS DERIVED, and what is deliberately not.
 *
 * IMPLEMENTED -- round transitions inside a thread. Highest value and free:
 * it is the only derivation that shows a conversation DEVELOPING, it needs no
 * proto change, and the page previously threw the information away by
 * printing a bare "(round N)" on every comment. Comments are grouped into
 * consecutive same-round runs under a round label, and a run whose round is
 * later than the round the thread was raised in is marked as such.
 *
 * IMPLEMENTED -- anchor grouping. Second, because it matches how a reviewer
 * reads ("what was said about this file") and the relationship is read off
 * the data rather than guessed. Note that `ListComments` is PAGINATED, so a
 * group is the threads on that file WITHIN THE CURRENT PAGE; the heading is
 * the file path alone and claims nothing more.
 *
 * IMPLEMENTED -- resolved threads collapse by default. Third, because it is a
 * visibility default rather than a relationship: a resolved thread is a
 * finished conversation, and an admin deciding on a proposal needs the
 * unfinished ones. Collapsing is per-thread and reversible, and an explicit
 * toggle always beats the default, including after a refetch.
 *
 * REJECTED -- cross-thread chaining. `Thread` has no parent, reply-to or
 * continuation field, so "this thread continues that one" could only ever be
 * inferred from proximity, and proximity is not the relation: two threads on
 * adjacent lines raised by different reviewers in the same round are two
 * independent remarks. A wrong guess here is WORSE than a flat list, because
 * a flat list asserts nothing while a drawn connection asserts something
 * false to the person deciding whether to accept the code. Nothing in this
 * component draws a line, a nesting level or a sequence number between two
 * threads.
 *
 * WHETHER THE PROTO SHOULD CARRY A PARENT LINK: not on this evidence. The
 * case that motivates one -- a follow-up thread that continues an earlier
 * conversation at a new location -- is already served by the two derivations
 * above (same file, later round), and adding a field means a proto change, a
 * migration, and a CLI surface for setting it, all to record something an
 * agent would have to remember to declare. If it is ever filed, it should be
 * filed with an example of a conversation these derivations genuinely cannot
 * show, not on principle.
 */
export function ThreadList({ threads }: ThreadListProps): ReactElement {
  // Explicit toggles only. The default is derived from `resolved` on every
  // render, so a thread that arrives (or gets resolved) on a poll picks up the
  // right default, while a thread the admin opened or closed by hand stays
  // the way they left it.
  const [overrides, setOverrides] = useState<ReadonlyMap<string, boolean>>(new Map());
  const isOpen = (thread: Thread): boolean => overrides.get(thread.id) ?? !thread.resolved;
  const setOpen = (thread: Thread, open: boolean): void => {
    setOverrides((previous) => {
      if (previous.get(thread.id) === open) return previous;
      const next = new Map(previous);
      next.set(thread.id, open);
      return next;
    });
  };

  return (
    <div className={styles.root}>
      {groupThreadsByAnchor(threads).map((group) => (
        <section key={group.file ?? ""} className={styles.group}>
          <h3 className={styles.groupHeading}>
            {group.file === undefined ? (
              "Not anchored to a file"
            ) : (
              <span className={styles.mono}>{group.file}</span>
            )}
          </h3>
          <ul className={styles.threadList}>
            {group.threads.map((thread) => (
              <li key={thread.id}>
                <details
                  className={styles.thread}
                  open={isOpen(thread)}
                  onToggle={(event) => setOpen(thread, event.currentTarget.open)}
                >
                  <summary className={styles.threadHeading}>
                    <span className={styles.mono}>{anchorLabel(thread)}</span>
                    <span className={styles.raised}>Raised in round {thread.round}</span>
                    {thread.resolved && <StatusBadge intent="success">Resolved</StatusBadge>}
                  </summary>
                  <ol className={styles.rounds}>
                    {roundRuns(thread.comments).map((run, index) => (
                      <li key={`${thread.id}-round-${index}`} className={styles.round}>
                        <p
                          className={
                            run.round > thread.round ? styles.roundLabelLater : styles.roundLabel
                          }
                        >
                          <span>Round {run.round}</span>
                          {run.round > thread.round && (
                            <span className={styles.laterNote}>
                              after the round this thread was raised in
                            </span>
                          )}
                        </p>
                        <ul className={styles.commentList}>
                          {run.comments.map((comment, commentIndex) => (
                            <li
                              key={`${thread.id}-${index}-${commentIndex}`}
                              className={styles.comment}
                            >
                              <p className={styles.commentMeta}>
                                <span className={styles.commentAuthor}>{comment.author}</span>
                              </p>
                              {/* Written by a DIFFERENT agent to the branch's
                                  author -- a reviewer, whose role is to be
                                  adversarial about this change -- and rendered
                                  in the console that accepts it. Same
                                  untrusted renderer as the description
                                  (components/Markdown.tsx). */}
                              <Markdown source={comment.body} />
                            </li>
                          ))}
                        </ul>
                      </li>
                    ))}
                  </ol>
                </details>
              </li>
            ))}
          </ul>
        </section>
      ))}
    </div>
  );
}
