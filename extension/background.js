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
      await showToast(tab.id, "Nothing to capture", "#8e8e93");
      return;
    }

    const response = await fetch(ENDPOINT, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });

    if (response.ok) {
      await flashBadge("✓", "#34c759");
      await showToast(tab.id, "Saved to Engrex", "#34c759");
    } else {
      await flashBadge("!", "#ff3b30");
      await showToast(tab.id, "Couldn't save to Engrex", "#ff3b30");
    }
  } catch (error) {
    // Most common cause: the Engrex daemon isn't running.
    await flashBadge("!", "#ff3b30");
    await showToast(tab.id, "Engrex daemon not reachable", "#ff3b30");
    console.error("Engrex capture failed:", error);
  }
}

// Injected into the page to show a brief floating confirmation popup.
function renderToast(message, color) {
  const toast = document.createElement("div");
  toast.textContent = message;
  Object.assign(toast.style, {
    position: "fixed",
    top: "20px",
    right: "20px",
    zIndex: "2147483647",
    padding: "12px 18px",
    background: "rgba(20, 20, 22, 0.92)",
    color: "#ffffff",
    font: "600 14px -apple-system, system-ui, sans-serif",
    borderRadius: "12px",
    boxShadow: "0 8px 24px rgba(0, 0, 0, 0.35)",
    borderLeft: "4px solid " + color,
    opacity: "0",
    transform: "translateY(-8px)",
    transition: "opacity 0.2s ease, transform 0.2s ease",
    pointerEvents: "none",
  });
  document.body.appendChild(toast);
  requestAnimationFrame(() => {
    toast.style.opacity = "1";
    toast.style.transform = "translateY(0)";
  });
  setTimeout(() => {
    toast.style.opacity = "0";
    toast.style.transform = "translateY(-8px)";
    setTimeout(() => toast.remove(), 250);
  }, 2000);
}

async function showToast(tabId, message, color) {
  if (tabId === undefined) {
    return;
  }
  try {
    await chrome.scripting.executeScript({
      target: { tabId: tabId },
      func: renderToast,
      args: [message, color],
    });
  } catch (error) {
    // Some pages (e.g. chrome:// or the web store) block injection — the badge
    // feedback still fires, so this is non-fatal.
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
