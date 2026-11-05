(function () {
  var TEXT = {
    tr: ["Runner Deck'ten çıkılsın mı?", "Çıkınca bu bilgisayardaki runner'lar durur, süren işler iptal olur. Pencereyi kapatırsan çalışmaya devam ederler.", 'Vazgeç', 'Çık'],
    en: ['Quit Runner Deck?', 'Quitting stops the runners on this computer and cancels any running job. Close the window instead to keep them working.', 'Cancel', 'Quit'],
    de: ['Runner Deck beenden?', 'Beim Beenden stoppen die Runner auf diesem Computer und laufende Jobs werden abgebrochen. Schließe nur das Fenster, damit sie weiterlaufen.', 'Abbrechen', 'Beenden'],
    fr: ['Quitter Runner Deck ?', 'Quitter arrête les runners de cet ordinateur et annule les jobs en cours. Fermez seulement la fenêtre pour les laisser tourner.', 'Annuler', 'Quitter'],
    zh: ['退出 Runner Deck？', '退出会停止这台电脑上的 Runner，并取消正在运行的任务。只关闭窗口则会继续运行。', '取消', '退出'],
    ko: ['Runner Deck을 종료할까요?', '종료하면 이 컴퓨터의 러너가 멈추고 실행 중인 작업이 취소됩니다. 창만 닫으면 계속 실행됩니다.', '취소', '종료'],
    ja: ['Runner Deck を終了しますか？', '終了するとこのコンピューターのランナーが停止し、実行中のジョブはキャンセルされます。ウィンドウを閉じるだけなら動作し続けます。', 'キャンセル', '終了']
  };
  var STOP = {
    tr: ["Runner'lar durdurulsun mu?", 'Süren işler iptal olur. Menü çubuğundan yeniden başlatabilirsin.', 'Vazgeç', 'Durdur'],
    en: ['Stop the runners?', 'Running jobs are canceled. You can start them again from the menu bar.', 'Cancel', 'Stop'],
    de: ['Runner stoppen?', 'Laufende Jobs werden abgebrochen. Du kannst sie über die Menüleiste neu starten.', 'Abbrechen', 'Stoppen'],
    fr: ['Arrêter les runners ?', "Les jobs en cours sont annulés. Vous pouvez les relancer depuis la barre de menus.", 'Annuler', 'Arrêter'],
    zh: ['停止 Runner？', '正在运行的任务会被取消。可以从菜单栏重新启动。', '取消', '停止'],
    ko: ['러너를 중지할까요?', '실행 중인 작업이 취소됩니다. 메뉴 막대에서 다시 시작할 수 있습니다.', '취소', '중지'],
    ja: ['ランナーを停止しますか？', '実行中のジョブはキャンセルされます。メニューバーから再び開始できます。', 'キャンセル', '停止']
  };
  var stopping = location.hash === '#stop';
  var lang = (navigator.language || 'en').slice(0, 2);
  var T = (stopping ? STOP : TEXT)[lang] || (stopping ? STOP : TEXT).en;
  var tauri = window.__TAURI__;
  document.getElementById('title').textContent = T[0];
  document.getElementById('body').textContent = T[1];
  var cancel = document.getElementById('cancel');
  var quit = document.getElementById('quit');
  cancel.textContent = T[2];
  quit.textContent = T[3];
  cancel.addEventListener('click', function () { tauri.window.getCurrentWindow().close(); });
  quit.addEventListener('click', function () { tauri.core.invoke(stopping ? 'stop_confirmed' : 'quit_confirmed'); });
  document.addEventListener('keydown', function (event) {
    if (event.key === 'Escape') cancel.click();
  });
  cancel.focus();
})();
