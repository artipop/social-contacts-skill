package tg

// The JavaScript here runs inside the user's own logged-in Telegram Web page and
// is where all the Telegram knowledge lives. It was tuned against the live
// site, and rewriting it would mean re-learning the same quirks. The Go around
// it only decides what to ask for and what to do with the answers.

// jsIsLoggedIn reports whether the page is past the login screen. A chat list
// and no login prompts is the test — Telegram Web has no single flag for it.
const jsIsLoggedIn = `() => {
    const loginMarkers = [
        ...document.querySelectorAll('input, button, div, span')
    ].some((el) => {
        const text = (el.textContent || '').toLowerCase();
        return (
            text.includes('log in') ||
            text.includes('sign in') ||
            text.includes('войти') ||
            text.includes('phone number') ||
            text.includes('номер телефона')
        );
    });
    const chatListExists = !!document.querySelector('.chatlist-container, .tabs-container');
    return chatListExists && !loginMarkers;
}`

// jsBridge is one async function dispatched by `op`. It talks to the live tweb
// manager layer (rootScope.managers) — the same MTProto access tweb itself uses
// — and returns plain JSON. Every Telegram-version quirk (object-vs-positional
// manager signatures, the user_id-vs-peer participant shape, usernames[] vs
// username) is handled here behind defensive fallbacks, so the Go side stays
// simple.
//
// This is why the collector is not DOM scraping: asking Telegram's own data
// layer is both faster and far less fragile than reading its rendered list.
const jsBridge = `
async (params) => {
  const rs = globalThis.rootScope;
  if (!rs || !rs.managers) return { ok: false, error: 'managers_not_ready' };
  const m = rs.managers;

  const errStr = (e) => {
    if (!e) return '';
    if (typeof e === 'string') return e;
    return String(e.type || e.message || e);
  };
  const callFirst = async (fns) => {
    let lastErr;
    for (const fn of fns) {
      try { return await fn(); } catch (e) { lastErr = e; }
    }
    throw lastErr || new Error('all_calls_failed');
  };
  // Telegram's "come back in N seconds" hint. tweb surfaces it as a FLOOD_WAIT_N
  // error type (or a numeric .seconds); we pass N up so the caller waits exactly
  // that long instead of guessing/backing off blindly.
  const floodOf = (e) => {
    if (e && typeof e.seconds === 'number' && e.seconds > 0) return e.seconds;
    const m = /FLOOD_WAIT_(\d+)/.exec(errStr(e));
    return m ? parseInt(m[1], 10) : 0;
  };
  const pickUsername = (o) => {
    if (!o) return '';
    if (o.username) return o.username;
    if (Array.isArray(o.usernames)) {
      const active = o.usernames.find((u) => u && (u.username) && (!u.pFlags || u.pFlags.active !== false));
      if (active) return active.username;
      if (o.usernames[0] && o.usernames[0].username) return o.usernames[0].username;
    }
    return '';
  };

  if (params.op === 'whoami') {
    const myId = rs.getMyId ? rs.getMyId() : 0;
    return { ok: myId > 0, my_id: String(myId) };
  }

  if (params.op === 'resolve') {
    const uname = String(params.username || '').replace(/^@/, '');
    let peer;
    try { peer = await m.appUsersManager.resolveUsername(uname); }
    catch (e) { return { ok: false, error: 'resolve_failed: ' + errStr(e) }; }
    if (!peer) return { ok: false, error: 'not_found' };
    const isUser = peer._ === 'user';
    return {
      ok: true,
      kind: peer._ || '',
      id: String(peer.id),
      title: peer.title || [peer.first_name, peer.last_name].filter(Boolean).join(' ') || '',
      username: pickUsername(peer),
      is_user: isUser,
      is_broadcast: !!(peer.pFlags && peer.pFlags.broadcast),
      is_megagroup: !!(peer.pFlags && peer.pFlags.megagroup),
    };
  }

  if (params.op === 'channel_full') {
    const chatId = String(params.chat_id);
    let full;
    try { full = await m.appProfileManager.getChannelFull(chatId, true); }
    catch (e) { return { ok: false, error: 'full_failed: ' + errStr(e) }; }
    if (!full) return { ok: false, error: 'full_empty' };
    return {
      ok: true,
      linked_chat_id: full.linked_chat_id ? String(full.linked_chat_id) : '',
      participants_count: full.participants_count != null ? full.participants_count : 0,
      can_view_participants: !!(full.pFlags && full.pFlags.can_view_participants),
    };
  }

  if (params.op === 'participants') {
    const chatId = String(params.chat_id);
    const limit = params.limit || 200;
    const offset = params.offset || 0;
    const q = params.q != null ? String(params.q) : null;
    const filter = q != null
      ? { _: 'channelParticipantsSearch', q }
      : { _: 'channelParticipantsRecent' };

    let res;
    try {
      res = await callFirst([
        () => m.appProfileManager.getParticipants({ id: chatId, filter, limit, offset }),
        () => m.appProfileManager.getParticipants(chatId, filter, limit, offset),
        () => m.appProfileManager.getChannelParticipants({ id: chatId, filter, limit, offset }),
        () => m.appProfileManager.getChannelParticipants(chatId, filter, limit, offset),
      ]);
    } catch (e) {
      return { ok: false, error: 'participants_failed: ' + errStr(e), flood_wait: floodOf(e) };
    }

    let count = 0;
    let participants = [];
    let users = [];
    if (Array.isArray(res)) {
      participants = res;
    } else if (res) {
      count = res.count != null ? res.count : 0;
      participants = res.participants || (res.participants && res.participants.participants) || [];
      users = res.users || [];
    }

    const userMap = {};
    for (const u of users) { if (u && u.id != null) userMap[String(u.id)] = u; }

    const uidOf = (p) => {
      if (!p) return null;
      if (p.user_id != null) return String(p.user_id);
      if (p.peer) {
        if (p.peer.user_id != null) return String(p.peer.user_id);
        if (p.peer.channel_id != null) return null; // peer is a channel, skip
      }
      return null;
    };

    const members = [];
    for (const p of participants) {
      const uid = uidOf(p);
      if (!uid) continue;
      let u = userMap[uid];
      if (!u) { try { u = await m.appUsersManager.getUser(uid); } catch (e) { /* skip */ } }
      const uname = pickUsername(u);
      members.push({
        user_id: uid,
        name: u ? [u.first_name, u.last_name].filter(Boolean).join(' ') : '',
        username: uname,
        phone: u && u.phone ? String(u.phone) : '',
        is_bot: !!(u && u.pFlags && u.pFlags.bot),
        is_premium: !!(u && u.pFlags && u.pFlags.premium),
        is_admin: p._ === 'channelParticipantAdmin' || p._ === 'channelParticipantCreator',
        role: p._ || '',
      });
    }
    return { ok: true, count, returned: participants.length, members };
  }

  if (params.op === 'user_full') {
    const uid = String(params.user_id);
    let full;
    try {
      full = await callFirst([
        () => m.appProfileManager.getProfile(uid),
        () => m.appProfileManager.getProfile(uid, true),
        () => m.appProfileManager.getProfileByPeerId(uid),
      ]);
    } catch (e) { return { ok: false, error: 'profile_failed: ' + errStr(e) }; }
    const about = (full && (full.about || (full.full_user && full.full_user.about))) || '';
    return { ok: true, about: String(about || '') };
  }

  return { ok: false, error: 'unknown_op' };
}
`
