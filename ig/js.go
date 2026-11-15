package ig

// The JavaScript here runs inside the user's own logged-in Instagram page.
// Instagram has no data layer to ask — unlike Telegram, whose managers we call
// directly — so this is the DOM, read as carefully as it can be: by role and by
// shape rather than by class names, which change constantly.

// jsPageState reports what the page actually is right now. Instagram answers a
// scrape with a captcha, a checkpoint, a rate-limit notice or a login wall far
// more often than with an error, and all of them render as an ordinary page —
// so "we got 0 followers" and "we were blocked" are indistinguishable without
// looking. Telling them apart is the difference between a user retrying
// pointlessly and a user being told to slow down.
const jsPageState = `() => {
    const url = (location.href || '').toLowerCase();
    const text = ((document.body && document.body.innerText) || '').slice(0, 4000).toLowerCase();
    const frames = Array.from(document.querySelectorAll('iframe')).map((f) => (f.src || '').toLowerCase());
    const captcha_frame = frames.some((s) =>
        s.includes('recaptcha') || s.includes('captcha') || s.includes('hcaptcha') ||
        s.includes('funcaptcha') || s.includes('arkose'));
    // A logged-out profile page shows the profile with a «Log in» button and
    // no form, so the form alone misses it. ds_user_id is set for a logged-in
    // session and readable from the page.
    const session = /(^|;\s*)ds_user_id=/.test(document.cookie || '');
    const login = !session || !!document.querySelector(
        'input[name="username"], input[name="password"], form[action*="/accounts/login"]');
    return { url, text, captcha_frame, login };
}`

// jsFollowerCandidates lists the page's clickable elements with short text,
// in document order, so Go can pick the followers count among them.
//
// Instagram renders the followers count as <a href="#" role="link">N followers</a>:
// the old /username/followers/ URL is gone and navigating it no longer opens the
// modal, so the only way in is to click the element whose text is the count. The
// following count sits right next to it and reads almost the same in Russian
// («подписчиков» / «подписок»), which is why the choice is made in Go, where it
// is tested.
const jsFollowerCandidates = `() => {
    const norm = (s) => (s || '').replace(/\s+/g, ' ').trim();
    return Array.from(document.querySelectorAll('a, [role="link"], [role="button"], button'))
        .map((n) => ({ text: norm(n.innerText).slice(0, 80), href: n.getAttribute('href') || '' }));
}`

// jsClickCandidate clicks the element at the given index of the list
// jsFollowerCandidates returned.
const jsClickCandidate = `(index) => {
    const nodes = Array.from(document.querySelectorAll('a, [role="link"], [role="button"], button'));
    const node = nodes[index];
    if (!node) return false;
    node.click();
    return true;
}`

// jsFindSearchInput reports whether the dialog's search box is present. That
// input, rather than the dialog element, is the real signal the modal is ready:
// the dialog appears before its contents do.
const jsFindSearchInput = `() => {
    const selectors = [
        'div[role="dialog"] input[placeholder*="Search" i]',
        'div[role="dialog"] input[placeholder*="Поиск" i]',
        'div[role="dialog"] input[type="text"]',
        'input[placeholder*="Search" i]',
        'input[placeholder*="Поиск" i]',
        'input[type="text"]',
    ];
    for (const selector of selectors) {
        const el = document.querySelector(selector);
        if (el && el.offsetParent !== null) return true;
    }
    return false;
}`

// jsTypeSearch puts a query into the dialog's search box.
//
// The value is set directly and the events React listens for are dispatched by
// hand: typing key by key is slower and no more reliable, and setting .value
// alone leaves React's state stale so the list never refreshes.
const jsTypeSearch = `(query) => {
    const selectors = [
        'div[role="dialog"] input[placeholder*="Search" i]',
        'div[role="dialog"] input[placeholder*="Поиск" i]',
        'div[role="dialog"] input[type="text"]',
        'input[placeholder*="Search" i]',
        'input[placeholder*="Поиск" i]',
        'input[type="text"]',
    ];
    let input = null;
    for (const selector of selectors) {
        const el = document.querySelector(selector);
        if (el && el.offsetParent !== null) { input = el; break; }
    }
    if (!input) return false;
    const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
    input.focus();
    setter.call(input, '');
    input.dispatchEvent(new Event('input', { bubbles: true }));
    setter.call(input, query);
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.dispatchEvent(new Event('change', { bubbles: true }));
    return true;
}`

// jsExtractProfiles reads the profiles currently listed in the dialog.
//
// A row is found from its profile link outward: the link is the only reliable
// anchor, so the walk goes up through parents until it finds the element that
// holds both the username and something else — that is the row, whatever
// Instagram calls its classes this week.
const jsExtractProfiles = `() => {
    const technical = new Set([
        'about', 'accounts', 'api', 'ar', 'blog', 'business', 'challenge', 'developer',
        'direct', 'directory', 'explore', 'graphql', 'help', 'invites', 'legal', 'oauth', 'p',
        'privacy', 'reel', 'reels', 'stories', 'terms', 'tv', 'web'
    ]);
    const dialog = document.querySelector('div[role="dialog"]') || document.body;
    const normalize = (value) => (value || '').replace(/\s+/g, ' ').trim();
    const result = [];
    const seen = new Set();

    for (const link of Array.from(dialog.querySelectorAll('a[href]'))) {
        let url;
        try {
            url = new URL(link.href, location.href);
        } catch {
            continue;
        }
        let host = url.hostname.toLowerCase();
        if (host.startsWith('www.')) host = host.slice(4);
        if (host !== 'instagram.com') continue;

        const parts = url.pathname.split('/').filter(Boolean);
        if (parts.length !== 1) continue;
        const username = (parts[0] || '').trim();
        if (!/^[A-Za-z0-9._]{1,30}$/.test(username)) continue;
        if (technical.has(username.toLowerCase())) continue;

        let container = link;
        for (let i = 0; i < 7; i += 1) {
            if (!container.parentElement) break;
            container = container.parentElement;
            const text = normalize(container.innerText || '');
            if (text && text.includes(username) && text.length > username.length) break;
        }

        const rowText = normalize(container.innerText || link.innerText || '');
        const lines = rowText.split('\n').map(normalize).filter(Boolean);
        const fullName = lines.find((line) => line !== username && !/^follow|following|подпис/i.test(line)) || '';
        const profileUrl = 'https://www.instagram.com/' + username + '/';
        if (seen.has(profileUrl)) continue;
        seen.add(profileUrl);
        result.push({
            username,
            full_name: fullName,
            profile_url: profileUrl,
            row_text: rowText,
        });
    }
    return result;
}`

// jsScrollDialog scrolls the followers list one screen.
//
// The scrollable element is found by measurement rather than by selector — the
// tallest thing inside the dialog whose content overflows it — because the
// dialog's structure changes and its scroller has no stable identity. It reports
// whether anything moved, which is how the caller knows the list has ended.
const jsScrollDialog = `() => {
    const dialog = document.querySelector('div[role="dialog"]');
    if (!dialog) return false;
    const scrollers = Array.from(dialog.querySelectorAll('*'))
        .filter((el) => el.scrollHeight > el.clientHeight + 20)
        .sort((a, b) => (b.scrollHeight - b.clientHeight) - (a.scrollHeight - a.clientHeight));
    const scroller = scrollers[0] || dialog;
    const previous = scroller.scrollTop;
    scroller.scrollTop = previous + Math.max(240, Math.floor(scroller.clientHeight * 0.85));
    return scroller.scrollTop !== previous;
}`
