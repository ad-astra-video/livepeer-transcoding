var currentUser = null;
var web3 = null;

function show_success(message) {
    let success_alert = document.querySelector("#success");
    let success_message = document.querySelector("#success-text");
    success_message.textContent = message;
    success_alert.classList.remove('visually-hidden');
    setTimeout(function(success_alert) {
        success_alert.classList.add('visually-hidden');
    }, 2000);
}

function show_failed(message) {
    let failed_alert = document.querySelector("#failed");
    let failed_message = document.querySelector("#failed-text");
    failed_message.textContent = message;
    failed_alert.classList.remove('visually-hidden');
    setTimeout(function(failed_alert) {
        failed_alert.classList.add('visually-hidden');
    }, 2000);
}