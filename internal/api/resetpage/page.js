(function () {
  var form = document.getElementById('reset-form');
  var message = document.getElementById('message');
  var password = document.getElementById('password');
  var confirm = document.getElementById('confirm');
  var button = form.querySelector('button');

  var token = new URLSearchParams(location.search).get('token');
  history.replaceState(null, '', location.pathname);

  function show(text, kind) {
    message.textContent = text;
    message.dataset.kind = kind;
  }

  var locked = false;

  function finish(text, kind) {
    show(text, kind);
    form.hidden = true;
  }

  function lock(text) {
    show(text, 'error');
    locked = true;
    password.disabled = true;
    confirm.disabled = true;
    button.disabled = true;
  }

  if (!token) {
    finish('This link is incomplete. Request a new reset mail.', 'error');
    return;
  }

  form.addEventListener('submit', function (event) {
    event.preventDefault();
    if (password.value !== confirm.value) {
      show('The two passwords do not match.', 'error');
      return;
    }
    show('', '');
    button.disabled = true;

    // Under this page's no-referrer policy a POST carries "Origin: null", which
    // the same-origin guard refuses; strict-origin sends the real origin only.
    fetch('/api/auth/reset-password', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: token, password: password.value }),
      credentials: 'same-origin',
      referrerPolicy: 'strict-origin'
    }).then(function (response) {
      if (response.ok) {
        finish('Your password has been changed. Sign in with your new password.', 'ok');
        return;
      }
      if (response.status === 429) {
        show('Too many attempts. Try again in a few minutes.', 'error');
        return;
      }
      return response.json().then(function (body) {
        var error = (body && body.error) || {};
        if (response.status === 400 && error.code === 'invalid_token') {
          lock(error.message || 'This link is no longer valid. Request a new reset mail.');
        } else if (response.status === 400 && error.code === 'invalid_value') {
          show(error.message || 'That password is not acceptable.', 'error');
        } else {
          throw new Error('unexpected response');
        }
      });
    }).catch(function () {
      show('Something went wrong. Try again.', 'error');
    }).then(function () {
      if (!locked) button.disabled = false;
    });
  });
})();
