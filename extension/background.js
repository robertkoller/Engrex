// Engrex Capture — posts the current selection (or full page) to the daemon's
// local HTTP endpoint, along with the page URL and title.

const ENDPOINT = "http://127.0.0.1:7777/capture";
const FILE_ENDPOINT = "http://127.0.0.1:7777/capture-file";

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

// Google Docs draws on a canvas and Slides on SVG, so innerText is useless there.
// Instead we pull the export the same way File > Download does, using the browser's
// Google session so private files work too. The optional /u/N/ is which signed in
// account the tab is using, so we fetch as that same account
const GOOGLE_EXPORTS = [
  {
    pattern: /^https:\/\/docs\.google\.com\/document\/(?:u\/(\d+)\/)?d\/([A-Za-z0-9_-]+)/,
    kind: "document",
    exportPath: "export?format=md",
    format: "markdown",
    titleSuffix: " - Google Docs",
  },
  {
    pattern: /^https:\/\/docs\.google\.com\/presentation\/(?:u\/(\d+)\/)?d\/([A-Za-z0-9_-]+)/,
    kind: "presentation",
    exportPath: "export/txt",
    format: "",
    titleSuffix: " - Google Slides",
  },
];

// Drive's file viewer only renders the pages you've scrolled to, so we download the
// actual file and let the daemon run it through the same extractors as a dropped file
const DRIVE_FILE_PATTERN = /^https:\/\/drive\.google\.com\/file\/(?:u\/(\d+)\/)?d\/([A-Za-z0-9_-]+)/;
const DRIVE_TITLE_SUFFIX = " - Google Drive";

class CaptureError extends Error {}

function trimSuffix(text, suffix) {
  if (text.endsWith(suffix)) {
    return text.slice(0, -suffix.length);
  }
  return text;
}

// A signed out or no access fetch gets redirected to a login/HTML page instead of failing
function checkGoogleResponse(response) {
  const contentType = response.headers.get("Content-Type") || "";
  if (response.url.startsWith("https://accounts.google.com/")) {
    throw new CaptureError("Google wants you to sign in");
  }
  if (!response.ok) {
    throw new CaptureError(`Google returned ${response.status}`);
  }
  if (contentType.includes("text/html")) {
    // Files over ~100MB get a "can't scan for viruses" page instead of the file
    throw new CaptureError("Google sent a web page, not the file (signed out, no access, or too big)");
  }
}

// Pulls the file name out of a Content-Disposition header, preferring the UTF-8 form
function fileNameFrom(contentDisposition) {
  if (!contentDisposition) {
    return "";
  }
  const encoded = contentDisposition.match(/filename\*=UTF-8''([^;]+)/i);
  if (encoded) {
    try {
      return decodeURIComponent(encoded[1].trim());
    } catch (error) {
      // Badly encoded, fall through to the plain filename
    }
  }
  const plain = contentDisposition.match(/filename="?([^";]+)"?/i);
  return plain ? plain[1].trim() : "";
}

// Builds the request to send to the daemon for a Docs/Slides tab. The url is trimmed
// down to the document ID so /edit, #slide=... etc all re-ingest as the same document
async function googleExportRequest(tab, googleExport, match) {
  const accountPath = match[1] ? `u/${match[1]}/` : "";
  const baseUrl = `https://docs.google.com/${googleExport.kind}/d/${match[2]}`;
  const response = await fetch(
    `https://docs.google.com/${googleExport.kind}/${accountPath}d/${match[2]}/${googleExport.exportPath}`,
    { credentials: "include" },
  );
  checkGoogleResponse(response);

  const text = (await response.text()).trim();
  if (!text) {
    return null;
  }
  return {
    url: ENDPOINT,
    options: {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        text: text,
        url: baseUrl,
        title: trimSuffix(tab.title || "", googleExport.titleSuffix),
        format: googleExport.format,
      }),
    },
  };
}

// Builds the request for a Drive file tab: the raw file goes in the body and the
// name/url/title ride along as query parameters
async function driveFileRequest(tab, match) {
  const accountQuery = match[1] ? `&authuser=${match[1]}` : "";
  const response = await fetch(
    `https://drive.google.com/uc?export=download&id=${match[2]}${accountQuery}`,
    { credentials: "include" },
  );
  checkGoogleResponse(response);

  const title = trimSuffix(tab.title || "", DRIVE_TITLE_SUFFIX);
  const name = fileNameFrom(response.headers.get("Content-Disposition")) || title;
  const parameters = new URLSearchParams({
    name: name,
    title: name,
    url: `https://drive.google.com/file/d/${match[2]}`,
  });
  return {
    url: `${FILE_ENDPOINT}?${parameters}`,
    options: {
      method: "POST",
      headers: { "Content-Type": "application/octet-stream" },
      body: await response.blob(),
    },
  };
}

// Builds the request from whatever's on the page for every other site
async function pageTextRequest(tab) {
  const [injection] = await chrome.scripting.executeScript({
    target: { tabId: tab.id },
    func: extractCapture,
  });
  const payload = injection && injection.result;
  if (!payload || !payload.text) {
    return null;
  }
  return {
    url: ENDPOINT,
    options: {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    },
  };
}

// Picks how to capture the tab and returns the daemon request, or null if there's nothing
async function buildRequest(tab) {
  const driveMatch = tab.url && tab.url.match(DRIVE_FILE_PATTERN);
  if (driveMatch) {
    return driveFileRequest(tab, driveMatch);
  }
  for (const googleExport of GOOGLE_EXPORTS) {
    const match = tab.url && tab.url.match(googleExport.pattern);
    if (match) {
      return googleExportRequest(tab, googleExport, match);
    }
  }
  return pageTextRequest(tab);
}

async function fail(tabId, message, error) {
  await flashBadge("!", "#ff3b30");
  await showToast(tabId, message, "#ff3b30");
  console.error("Engrex capture failed:", error || message);
}

async function capture(tab) {
  if (!tab || tab.id === undefined) {
    return;
  }

  let request;
  try {
    request = await buildRequest(tab);
  } catch (error) {
    if (error instanceof CaptureError) {
      await fail(tab.id, `Couldn't get it from Google: ${error.message}`, error);
    } else {
      await fail(tab.id, "Couldn't read this page", error);
    }
    return;
  }

  if (!request) {
    await flashBadge("∅", "#8e8e93"); // nothing to capture
    await showToast(tab.id, "Nothing to capture", "#8e8e93");
    return;
  }

  let response;
  try {
    response = await fetch(request.url, request.options);
  } catch (error) {
    // Most common cause: the Engrex daemon isn't running.
    await fail(tab.id, "Engrex daemon not reachable", error);
    return;
  }

  if (response.ok) {
    await flashBadge("✓", "#34c759");
    await showToast(tab.id, "Saved to Engrex", "#34c759");
  } else if (response.status === 415) {
    await fail(tab.id, "Engrex can't read this file type");
  } else if (response.status === 422) {
    await fail(tab.id, "No readable text in this file");
  } else {
    await fail(tab.id, "Couldn't save to Engrex");
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
