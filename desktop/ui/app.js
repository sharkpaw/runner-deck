(function () {
  var TEXT = {
    tr: { sign_in: 'Giriş yapmak için tıkla, tarayıcı açılır', connections: 'Bağlantılar', add_server: 'Sunucu ekle', name: 'Ad', address: 'Panel adresi', add: 'Ekle', remove: 'Kaldır', local: 'Bu bilgisayar', local_hint: "Runner'lar burada normal süreç olarak çalışır", starting: 'Açılıyor…', https_required: 'Adres https:// ile başlamalı.', invalid_url: 'Bu adres geçerli değil.' },
    en: { sign_in: 'Click to sign in, your browser opens', connections: 'Connections', add_server: 'Add a server', name: 'Name', address: 'Panel address', add: 'Add', remove: 'Remove', local: 'This computer', local_hint: 'Runners work here as normal processes', starting: 'Opening…', https_required: 'The address must start with https://.', invalid_url: 'This address is not valid.' },
    de: { sign_in: 'Zum Anmelden klicken, der Browser öffnet sich', connections: 'Verbindungen', add_server: 'Server hinzufügen', name: 'Name', address: 'Panel-Adresse', add: 'Hinzufügen', remove: 'Entfernen', local: 'Dieser Computer', local_hint: 'Runner laufen hier als normale Prozesse', starting: 'Wird geöffnet…', https_required: 'Die Adresse muss mit https:// beginnen.', invalid_url: 'Diese Adresse ist ungültig.' },
    fr: { sign_in: 'Cliquez pour vous connecter, le navigateur s\'ouvre', connections: 'Connexions', add_server: 'Ajouter un serveur', name: 'Nom', address: 'Adresse du panneau', add: 'Ajouter', remove: 'Retirer', local: 'Cet ordinateur', local_hint: 'Les runners tournent ici comme des processus normaux', starting: 'Ouverture…', https_required: "L'adresse doit commencer par https://.", invalid_url: "Cette adresse n'est pas valide." },
    zh: { sign_in: '点击登录，将打开浏览器', connections: '连接', add_server: '添加服务器', name: '名称', address: '面板地址', add: '添加', remove: '移除', local: '这台电脑', local_hint: 'Runner 在这里以普通进程运行', starting: '正在打开…', https_required: '地址必须以 https:// 开头。', invalid_url: '此地址无效。' },
    ko: { sign_in: '클릭하면 브라우저에서 로그인합니다', connections: '연결', add_server: '서버 추가', name: '이름', address: '패널 주소', add: '추가', remove: '제거', local: '이 컴퓨터', local_hint: '러너가 여기서 일반 프로세스로 실행됩니다', starting: '여는 중…', https_required: '주소는 https://로 시작해야 합니다.', invalid_url: '올바른 주소가 아닙니다.' },
    ja: { sign_in: 'クリックするとブラウザーでログインします', connections: '接続', add_server: 'サーバーを追加', name: '名前', address: 'パネルのアドレス', add: '追加', remove: '削除', local: 'このコンピューター', local_hint: 'ランナーはここで通常のプロセスとして動作します', starting: '開いています…', https_required: 'アドレスは https:// で始めてください。', invalid_url: 'このアドレスは無効です。' }
  };
  var T = TEXT[(navigator.language || 'en').slice(0, 2)] || TEXT.en;
  var invoke = window.__TAURI__.core.invoke;
  var errorBox = document.getElementById('error');
  var list = document.getElementById('list');

  document.querySelectorAll('[data-t]').forEach(function (node) { node.textContent = T[node.dataset.t]; });

  function showError(err) {
    var text = String(err);
    errorBox.textContent = T[text] || text;
    errorBox.hidden = false;
  }

  function row(title, hint, open, remove) {
    var li = document.createElement('li');
    var button = document.createElement('button');
    button.className = 'open';
    button.type = 'button';
    var b = document.createElement('b');
    b.textContent = title;
    var small = document.createElement('small');
    small.textContent = hint;
    button.append(b, small);
    button.addEventListener('click', function () {
      errorBox.hidden = true;
      small.textContent = T.starting;
      open().catch(function (err) { small.textContent = hint; showError(err); });
    });
    li.append(button);
    if (remove) {
      var del = document.createElement('button');
      del.className = 'remove';
      del.type = 'button';
      del.textContent = T.remove;
      del.addEventListener('click', remove);
      li.append(del);
    }
    return li;
  }

  function render(state) {
    list.replaceChildren(row(T.local, T.local_hint, function () { return invoke('open_local'); }));
    state.servers.forEach(function (server) {
      list.append(row(server.name, server.signed_in ? server.url : T.sign_in, function () { return invoke('open_server', { id: server.id }); }, function () {
        invoke('remove_server', { id: server.id }).then(load).catch(showError);
      }));
    });
  }

  function load() {
    return invoke('connections').then(function (state) {
      render(state);
      // The list stays on screen when asked for from the menu, or when the first connection fails.
      // A browser must not open by itself at launch, so only a signed-in server opens directly.
      if (state.first_load && location.hash !== '#list') {
        var last = state.servers.find(function (s) { return s.id === state.last; });
        if (state.last === 'local') invoke('open_local').catch(showError);
        else if (last && last.signed_in) invoke('open_server', { id: last.id }).catch(showError);
      }
    }).catch(showError);
  }

  document.getElementById('add').addEventListener('submit', function (event) {
    event.preventDefault();
    errorBox.hidden = true;
    var name = document.getElementById('name');
    var url = document.getElementById('url');
    invoke('add_server', { name: name.value, url: url.value }).then(function () {
      name.value = '';
      url.value = '';
      return load();
    }).catch(showError);
  });

  load();
})();
