// Engrex Capture — posts the current selection (or full page) to the daemon's
// local HTTP endpoint, along with the page URL and title.

const ENDPOINT = "http://127.0.0.1:7777/capture";

// Runs inside the page. Returns the selected text, or the whole page's text if
// nothing is selected, plus the page URL and title.
function extractCapture() {
  const selection = window.getSelection().toString().trim();
  const text = selection.length > 0 ? selection : document.body.innerText.trim();
  return {
    text: text,
    url: window.location.href,
    title: document.title,
  };
}

async function capture(tab) {
  if (!tab || tab.id === undefined) {
    return;
  }

  try {
    const [injection] = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      func: extractCapture,
    });

    const payload = injection && injection.result;
    if (!payload || !payload.text) {
      await flashBadge("∅", "#8e8e93"); // nothing to capture
      return;
    }

    const response = await fetch(ENDPOINT, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });

    if (response.ok) {
      await flashBadge("✓", "#34c759");
    } else {
      await flashBadge("!", "#ff3b30");
    }
  } catch (error) {
    // Most common cause: the Engrex daemon isn't running.
    await flashBadge("!", "#ff3b30");
    console.error("Engrex capture failed:", error);
  }
}

// Briefly shows a colored badge on the toolbar icon as feedback.
async function flashBadge(text, color) {
  await chrome.action.setBadgeBackgroundColor({ color: color });
  await chrome.action.setBadgeText({ text: text });
  setTimeout(() => chrome.action.setBadgeText({ text: "" }), 1500);
}

// Keyboard shortcut (Cmd/Ctrl+Shift+E by default).
chrome.commands.onCommand.addListener(async (command) => {
  if (command === "capture") {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    await capture(tab);
  }
});

// Clicking the toolbar icon also captures.
chrome.action.onClicked.addListener((tab) => capture(tab));
