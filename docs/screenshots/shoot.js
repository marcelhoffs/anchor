// Seeds a throwaway Anchor demo with made-up users and public websites, then
// takes the README screenshots into ./out. Never point it at a real instance.
//
//   docker build -t anchor:demo .
//   docker run -d --name anchor-demo -p 127.0.0.1:8090:8080 -v anchor-demo:/data \
//     -e ANCHOR_BASE_URL=http://127.0.0.1:8090 -e ANCHOR_SECRET_KEY=$(openssl rand -base64 32) anchor:demo
//   for u in emma liam noah; do a=; [ $u = emma ] && a=--admin
//     echo "$u-demo-password" | docker exec -i anchor-demo /anchor user create $u $a --password-stdin; done
//   mkdir -p docs/screenshots/out && chmod 777 docs/screenshots/out
//   docker run --rm --network host -v "$PWD/docs/screenshots":/s -e NODE_PATH=/home/pptruser/node_modules \
//     ghcr.io/puppeteer/puppeteer:23.10.4 node /s/shoot.js
//   docker rm -f anchor-demo && docker volume rm anchor-demo
const puppeteer = require("puppeteer");

const BASE = "http://127.0.0.1:8090";
const OUT = "/s/out";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const personal = {
  tags: ["Homelab", "Development", "Reading", "News", "Recipes", "Travel"],
  bookmarks: [
    ["https://go.dev/", "The Go Programming Language", "Build simple, secure, scalable systems with Go.", "Development", true],
    ["https://github.com/", "GitHub", "Where the world builds software.", "Development"],
    ["https://www.sqlite.org/", "SQLite", "A small, fast, self-contained SQL database engine.", "Development", true],
    ["https://developer.mozilla.org/", "MDN Web Docs", "Documentation for web developers.", "Development, Reading"],
    ["https://www.home-assistant.io/", "Home Assistant", "Open source home automation that puts local control first.", "Homelab", true],
    ["https://jellyfin.org/", "Jellyfin", "The free software media system.", "Homelab"],
    ["https://www.proxmox.com/", "Proxmox", "Open-source virtualization and backup.", "Homelab"],
    ["https://pi-hole.net/", "Pi-hole", "Network-wide ad blocking.", "Homelab"],
    ["https://arstechnica.com/", "Ars Technica", "Technology news and analysis.", "News, Reading"],
    ["https://www.theguardian.com/", "The Guardian", "Independent journalism.", "News"],
    ["https://en.wikipedia.org/", "Wikipedia", "The free encyclopedia.", "Reading"],
    ["https://www.gutenberg.org/", "Project Gutenberg", "Over 70,000 free ebooks.", "Reading"],
    ["https://www.seriouseats.com/", "Serious Eats", "Recipes and the science of cooking.", "Recipes"],
    ["https://www.lonelyplanet.com/", "Lonely Planet", "Travel guides and inspiration.", "Travel"],
  ],
};

const family = {
  tags: ["Holidays", "School", "Recipes"],
  bookmarks: [
    ["https://www.khanacademy.org/", "Khan Academy", "Free lessons in maths, science and more.", "School"],
    ["https://www.duolingo.com/", "Duolingo", "Learn a language for free.", "School"],
    ["https://www.bbcgoodfood.com/", "BBC Good Food", "Recipes for every occasion.", "Recipes", true],
    ["https://www.nationalgeographic.com/travel", "National Geographic Travel", "Ideas for the next family trip.", "Holidays"],
  ],
};

// post submits a form the way the page would, with its CSRF token.
async function post(page, path, fields) {
  return page.evaluate(
    async (path, fields) => {
      const body = new URLSearchParams(fields);
      body.set("csrf", document.querySelector('meta[name="csrf-token"]').content);
      const r = await fetch(path, { method: "POST", body, redirect: "follow" });
      return r.url;
    },
    path,
    fields
  );
}

async function login(page, user) {
  await page.goto(BASE + "/login");
  await page.type('input[name="username"]', user);
  await page.type('input[name="password"]', user + "-demo-password");
  await Promise.all([page.waitForNavigation(), page.click('form[action="/login"] button')]);
}

async function seed(page, set, space) {
  for (const t of set.tags) await post(page, "/tags", { name: t });
  for (const [url, title, description, tags, fav] of set.bookmarks) {
    const f = { url, title, description, tags };
    if (fav) f.favourite = "1";
    if (space) f.space = space;
    await post(page, "/bookmarks", f);
  }
}

async function appearance(page, accent, theme) {
  await page.goto(BASE + "/settings");
  await post(page, "/settings/appearance", { accent, theme, sidebar: "left", sort: "newest", descriptions: "show" });
}

(async () => {
  const browser = await puppeteer.launch({ args: ["--no-sandbox"] });
  const page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 900, deviceScaleFactor: 1.5 });

  await login(page, "emma");
  await page.goto(BASE + "/");
  await seed(page, personal);

  // A shared space with an editor and a reader.
  const spaceURL = await post(page, "/spaces", { name: "Family" });
  const spaceID = spaceURL.match(/\/spaces\/(\d+)/)[1];
  await post(page, `/spaces/${spaceID}/members`, { username: "liam", role: "write" });
  await post(page, `/spaces/${spaceID}/members`, { username: "noah", role: "read" });
  // Tags are created in the space the session looks at: switch first.
  await post(page, "/space", { space: spaceID });
  await seed(page, family, spaceID);
  await post(page, "/space", { space: "" });

  console.log("seeded; waiting for page details and icons");
  await sleep(180000);

  // 1. The list, light.
  await appearance(page, "indigo", "light");
  await post(page, "/space", { space: "" });
  await page.goto(BASE + "/");
  await page.screenshot({ path: OUT + "/bookmarks.png" });

  // 2. A tag filter, dark.
  await appearance(page, "emerald", "dark");
  await page.goto(BASE + "/?q=%23Homelab");
  await page.screenshot({ path: OUT + "/dark.png" });

  // 3. The space switcher, in the shared space.
  await appearance(page, "indigo", "light");
  await post(page, "/space", { space: spaceID });
  await page.goto(BASE + "/");
  await page.click("details.space-menu > summary");
  await sleep(300);
  await page.screenshot({ path: OUT + "/spaces.png", clip: { x: 0, y: 0, width: 1440, height: 640 } });

  // 4. The shared space's members.
  await page.goto(BASE + `/spaces/${spaceID}`);
  await page.screenshot({ path: OUT + "/members.png" });

  // 5. On a phone.
  await post(page, "/space", { space: "" });
  await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 2, isMobile: true, hasTouch: true });
  await page.goto(BASE + "/");
  await page.screenshot({ path: OUT + "/phone.png" });

  await browser.close();
  console.log("done");
})().catch((e) => {
  console.error(e);
  process.exit(1);
});
