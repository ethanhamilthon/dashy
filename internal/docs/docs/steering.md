# Steering

While a turn is active, submitted text is queued as a steering instruction instead of starting an
overlapping request. The agent accepts queued instructions in FIFO order at a safe boundary, after all
tool calls and outputs of the current response are committed. Shell calls run one at a time, and only
after the complete API response. Argument previews are never run as partial calls.

The queue holds at most 64 instructions. A full queue rejects the new one (`queue_full` in the JSON
transport), and the text stays in the composer.

The interface shows pending instructions. Up on an empty composer recalls the newest unaccepted
instruction, unless completion navigation has priority. Recall and acceptance are synchronized: an
accepted instruction cannot be recalled or delivered twice. Unaccepted instructions and drafts are kept
for recovery after a cancellation or a late continuation.

Cancelling stops the active turn and terminates the running command's process group. Unaccepted
instructions and drafts are kept for recovery. `dashy view` queues images for the same safe boundary
(`dashy docs images`), and `dashy notify` delivery is described in `dashy docs headless`.

The JSON transport follows the same rules. `input` during a turn is queued as steering, and `steer` is
the explicit form, rejected when no turn runs. `recall` returns the newest unaccepted instruction and
fails once it has been accepted. `stop` cancels the active turn, and its final `done` lists the
unaccepted instructions in `unsent` so a client can resubmit them. Acceptance is reported by a `user`
event with `steering: true`. The request table is in `dashy docs headless`.
