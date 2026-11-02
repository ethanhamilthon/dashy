# Clipboard image attachments

The terminal never displays image pixels, Sixel, Kitty graphics, or base64 data. The composer shows only
an `[Image N: filename]` label. Ctrl+X removes the last draft attachment and its captured file. Enter
submits text and attached images, and an image-only message also works. The image bytes reach the
model, not just a path.

Ctrl+V reads the local clipboard asynchronously with a three-second timeout. On macOS, osascript's
AppKit bridge converts native clipboard images to PNG; ordinary text falls back to pbpaste. On Linux,
wl-paste (Wayland) or xclip (X11) is required; PNG clipboard images and ordinary text are supported.
Nothing is downloaded or installed. Missing helpers and clipboard failures show a notice or error and
never clear the draft. Ordinary terminal (bracketed) paste keeps working. Text pasted into provider-key
entry stays masked.

Cmd+V works when the terminal forwards it to Dashy. Many terminals consume it first. In that case use
Ctrl+V, or map Cmd+V to send Ctrl+V (0x16). Recognising Cyrillic М/м on the physical V key needs a
terminal that reports modifier and key codes, such as Kitty CSI-u. English and Russian key codes and
alternate base-layout codes are both recognized. Plain М/м stays text, not a paste shortcut. Key
release and repeat events do not add attachments. A legacy terminal cannot send modifiers it does not transmit.

Clipboard captures in the TUI are written to `attachment_dir` (see `dashy docs config`; the default is
`dashy-images` in the platform temporary directory). Headless `images` use the same key with a different
default, as described in `dashy docs config`. New directories use 0700 and files 0600. Images pass the same
validation as `dashy view` (PNG or JPEG, limits in `dashy docs images`), and only previews are sent to
the provider and kept in history. At most eight images can be pending across the active input, queued
instructions and the current turn.

Attachments follow their instruction through steering, recall, late continuation and cancellation
recovery. A turn error or cancellation restores even image-only input. Accepted messages are not resent
on cancellation. Pending images must be submitted or removed before switching sessions. Late clipboard
results from a previous session or a closed picker are discarded. Idle external notifications never
submit images from an unsent draft.

Captured originals stay on disk after submission or exit, for manual cleanup. Removing a draft image
removes its file. Rejected or stale captures are deleted. Session history embeds the image bytes, so
deleting the files later does not break a restored conversation. Unsubmitted drafts are not kept across
application exit. Clipboard content may be sensitive: both the files and the accepted history contain the image data.

Automated tests use synthetic clipboard data and never read the user's clipboard. Real clipboard access
and terminal shortcut forwarding need interactive checks on the target desktop.
