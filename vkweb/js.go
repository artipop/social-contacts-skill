package vkweb

// The JavaScript here runs inside the user's own logged-in VK page. The
// people search is a VKUI app whose class names are generated, so rows are
// found from what stays put: every result links its profile with a
// search_track_code, and the link that carries text is the name.

// jsPageState reports what the page is: where it is, the start of its text,
// the logged-in user's id (0 when logged out) and whether a captcha is shown.
const jsPageState = `() => {
    const url = (location.href || '').toLowerCase();
    const text = ((document.body && document.body.innerText) || '').slice(0, 4000).toLowerCase();
    const frames = Array.from(document.querySelectorAll('iframe')).map((f) => (f.src || '').toLowerCase());
    const captcha_frame = frames.some((s) => s.includes('captcha') || s.includes('not_robot')) ||
        !!document.querySelector('img[src*="captcha"], [class*="captcha" i]');
    const user_id = Number((window.vk && window.vk.id) || 0);
    return { url, text, user_id, captcha_frame };
}`

// jsResults reads the people listed on the search page: each row's profile
// path, the name as shown and the row's lines of text. empty is VK's own "found
// nothing", which tells an answer from a page that has not loaded. Rows marked
// stale by jsApplyFilters are the previous results and are skipped.
const jsResults = `() => {
    const norm = (s) => (s || '').replace(/[ \t\u00a0]+/g, ' ').trim();
    const text = (document.body && document.body.innerText) || '';
    const empty = /ничего\s+не\s+найдено|nothing\s+found|no\s+results/i.test(text);
    const rows = [];
    const seen = new Set();
    for (const link of Array.from(document.querySelectorAll('a[href*="search_track_code"]:not([data-sc-stale])'))) {
        const name = norm(link.innerText);
        if (!name) continue;
        let path;
        try {
            path = new URL(link.getAttribute('href'), location.href).pathname;
        } catch {
            continue;
        }
        if (seen.has(path)) continue;
        seen.add(path);
        let row = link;
        while (row.parentElement && !row.querySelector('img')) row = row.parentElement;
        const lines = (row.innerText || '').split('\n').map(norm).filter(Boolean);
        rows.push({ path, name, lines });
    }
    return { rows, empty };
}`

// jsApplyFilters sets the search's place of work and city the way a person
// does, and applies them: VK ignores filters in the address of a freshly
// loaded page. The city is typed into its box and the option whose first line
// is exactly that name is picked (VK lists the main city first). The rows on
// screen are marked stale first, so the caller does not read them as the
// filtered results.
const jsApplyFilters = `async (filters) => {
    const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
    const fold = (s) => (s || '').toLowerCase().replace(/ё/g, 'е').replace(/\s+/g, ' ').trim();
    const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
    const type = (input, value) => {
        input.focus();
        input.click();
        setter.call(input, value);
        input.dispatchEvent(new Event('input', { bubbles: true }));
        input.dispatchEvent(new Event('change', { bubbles: true }));
    };
    if (filters.company) {
        const input = Array.from(document.querySelectorAll('input'))
            .find((i) => /укажите место/i.test(i.placeholder || ''));
        if (!input) return { ok: false, error: 'нет поля «Работа»' };
        type(input, filters.company);
        await sleep(300);
    }
    if (filters.city) {
        const input = document.getElementById('city_select');
        if (!input) return { ok: false, error: 'нет поля города' };
        type(input, filters.city);
        let option = null;
        let seen = [];
        for (let i = 0; i < 20 && !option; i += 1) {
            await sleep(250);
            const options = Array.from(document.querySelectorAll('[role="option"]'));
            seen = options.slice(0, 5).map((o) => (o.innerText || '').split('\n')[0].trim());
            option = options.find((o) => fold((o.innerText || '').split('\n')[0]) === fold(filters.city)) || null;
        }
        if (!option) return { ok: false, unknown_city: true, error: 'VK не знает город, варианты: ' + seen.join('; ') };
        option.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
        option.click();
        await sleep(400);
    }
    const apply = Array.from(document.querySelectorAll('button'))
        .find((b) => /^(применить|apply)$/i.test((b.innerText || '').trim()));
    if (!apply) return { ok: false, error: 'нет кнопки «Применить»' };
    for (const link of Array.from(document.querySelectorAll('a[href*="search_track_code"]'))) {
        link.setAttribute('data-sc-stale', '1');
    }
    apply.click();
    const want = filters.city ? /[?&]city_id=\d+/ : /[?&]company=/;
    for (let i = 0; i < 20; i += 1) {
        await sleep(250);
        if (want.test(location.href)) return { ok: true, url: location.href };
    }
    return { ok: false, error: 'фильтр не применился' };
}`
