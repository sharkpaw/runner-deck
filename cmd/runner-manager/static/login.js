(function () {
  var langSelect = document.getElementById('langSelect');
  langSelect.addEventListener('change', function () {
    document.cookie = 'lang=' + encodeURIComponent(this.value) + ';path=/;max-age=31536000';
    location.reload();
  });

  var themeSelect = document.getElementById('themeSelect');
  var media = window.matchMedia('(prefers-color-scheme: dark)');
  function storedTheme() {
    try { return localStorage.getItem('runnerDeckTheme') || 'system'; } catch (_) { return 'system'; }
  }
  function applyTheme(mode) {
    var dark = mode === 'dark' || (mode !== 'light' && media.matches);
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
  }
  themeSelect.value = storedTheme();
  themeSelect.addEventListener('change', function () {
    try {
      if (this.value === 'system') localStorage.removeItem('runnerDeckTheme');
      else localStorage.setItem('runnerDeckTheme', this.value);
    } catch (_) {}
    applyTheme(this.value);
  });

  var toggle = document.getElementById('togglePass');
  var pass = document.getElementById('loginPass');
  toggle.addEventListener('click', function () {
    var show = pass.type === 'password';
    pass.type = show ? 'text' : 'password';
    toggle.setAttribute('aria-pressed', show ? 'true' : 'false');
    toggle.setAttribute('aria-label', show ? toggle.dataset.hide : toggle.dataset.show);
  });

  var features = document.getElementById('loginFeatures');
  var list = features.querySelector('ul');
  var dots = features.querySelector('.login-dots');
  var count = list.children.length;
  var current = 0;
  var paused = false;
  function show(i) {
    current = i;
    list.style.transform = 'translateX(-' + i * 100 + '%)';
    Array.prototype.forEach.call(dots.children, function (d, n) { d.classList.toggle('active', n === i); });
  }
  for (var i = 0; i < count; i++) {
    (function (n) {
      var dot = document.createElement('button');
      dot.type = 'button';
      dot.setAttribute('aria-label', list.children[n].querySelector('b').textContent);
      dot.addEventListener('click', function () { show(n); });
      dots.appendChild(dot);
    })(i);
  }
  show(0);
  features.addEventListener('mouseenter', function () { paused = true; });
  features.addEventListener('mouseleave', function () { paused = false; });
  if (!window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
    setInterval(function () { if (!paused) show((current + 1) % count); }, 4500);
  }
})();
